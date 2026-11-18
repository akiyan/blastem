package control

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type ProtocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ProtocolError) Error() string { return "BlastEM: " + e.Code + ": " + e.Message }

type SaveResult struct {
	Bytes  uint32 `json:"bytes"`
	Frame  uint64 `json:"frame"`
	Paused bool   `json:"paused"`
}

// A short, separate v2 connection keeps legacy command streams unmixed.
// Serializing with the existing client mutex also orders input and flush calls.
func (c *Client) callV2(command string, capability string, out any) error {
	return c.callV2Params(command, capability, map[string]any{}, out)
}

func (c *Client) callV2Params(command string, capability string, params map[string]any, out any) error {
	return c.callV2ParamsContext(context.Background(), command, capability, params, out)
}

func (c *Client) callV2ParamsContext(ctx context.Context, command string, capability string, params map[string]any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	if err := c.connectLocked(); err != nil {
		return err
	}
	v := newV2Transport(c.conn)
	cancelled := make(chan struct{})
	defer close(cancelled)
	go func() {
		select {
		case <-ctx.Done():
			v.close(ctx.Err())
		case <-cancelled:
		}
	}()
	defer func() {
		c.v2mu.Lock()
		if c.v2active == v {
			c.v2active = nil
		}
		c.v2mu.Unlock()
		v.close(fmt.Errorf("v2 request completed"))
		c.conn = nil
	}()
	var hello struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := v.request("hello", map[string]any{"client": "blastem-mcp"}, &hello, c.timeout); err != nil {
		return fmt.Errorf("core does not support acknowledged v2 control: %w", err)
	}
	found := false
	for _, cap := range hello.Capabilities {
		if cap == capability {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("core lacks %s (has %s)", capability, strings.Join(hello.Capabilities, ","))
	}
	if command == "hello" {
		return nil
	}
	if command == "advance" || command == "trace" || command == "line_trace" {
		timeout := 60 * time.Second
		if frames, ok := params["frames"].(uint32); ok && command != "advance" {
			// Allow 25% speed (~67 ms/frame) plus start-up for long traces.
			timeout += time.Duration(frames) * 70 * time.Millisecond
		}
		return v.requestReady(command, params, out, timeout, func() {
			c.v2mu.Lock()
			c.v2active = v
			c.v2mu.Unlock()
		})
	}
	return v.request(command, params, out, c.timeout)
}

func (c *Client) RequireSaveDirectory() error {
	return c.callV2("hello", "save_directory", nil)
}

func (c *Client) FlushSRAM() (SaveResult, error) {
	var result SaveResult
	err := c.callV2("save_flush", "sram_flush", &result)
	return result, err
}

// SnapshotResult describes the cached image separately from the stopped CPU frame.
type SnapshotResult struct {
	Width         uint32 `json:"width"`
	Height        uint32 `json:"height"`
	Sequence      uint64 `json:"sequence"`
	Frame         uint64 `json:"frame"`
	ImageBoundary string `json:"image_boundary"`
	Paused        bool   `json:"paused"`
}

func (c *Client) Snapshot(path string) (SnapshotResult, error) {
	var result SnapshotResult
	err := c.callV2Params("snapshot", "cached_screenshot", map[string]any{"path": path}, &result)
	return result, err
}

type FrameResult struct {
	Frame       uint32 `json:"frame"`
	Advanced    uint32 `json:"advanced"`
	Paused      bool   `json:"paused"`
	Interrupted bool   `json:"interrupted"`
	Boundary    string `json:"boundary"`
}

func (c *Client) Pause() (FrameResult, error) {
	var result FrameResult
	err := c.callV2("pause", "frame_control", &result)
	return result, err
}

func (c *Client) Advance(frames uint32) (FrameResult, error) {
	return c.AdvanceContext(context.Background(), frames)
}

func (c *Client) AdvanceContext(ctx context.Context, frames uint32) (FrameResult, error) {
	var result FrameResult
	if frames < 1 || frames > 600 {
		return result, fmt.Errorf("frames must be in 1..600")
	}
	err := c.callV2ParamsContext(ctx, "advance", "frame_control", map[string]any{"frames": frames}, &result)
	return result, err
}

// Interrupt bypasses the normal operation mutex and uses the active v2 stream.
// Receipt acknowledges the request; Advance returns the final stopped state.
func (c *Client) Interrupt() error {
	c.v2mu.Lock()
	active := c.v2active
	c.v2mu.Unlock()
	if active == nil {
		return fmt.Errorf("no frame run is active")
	}
	return active.request("interrupt", map[string]any{}, nil, c.timeout)
}

type AudioResult struct {
	SampleRate uint32 `json:"sample_rate"`
	Channels   uint32 `json:"channels"`
	Samples    uint64 `json:"samples"`
	Limited    bool   `json:"limited"`
	Boundary   string `json:"boundary"`
}

func (c *Client) StartAudio(path string, seconds uint32) (AudioResult, error) {
	var result AudioResult
	err := c.callV2Params("audio_start", "host_audio_capture", map[string]any{"path": path, "max_seconds": seconds}, &result)
	return result, err
}
func (c *Client) StopAudio() (AudioResult, error) {
	var result AudioResult
	err := c.callV2("audio_stop", "host_audio_capture", &result)
	return result, err
}

// Speed affects host pacing and audio resampling; frame counts retain their meaning.
type SpeedResult struct {
	Percent uint32 `json:"percent"`
	Frame   uint64 `json:"frame"`
	Paused  bool   `json:"paused"`
}

func (c *Client) SetSpeed(percent uint32) (SpeedResult, error) {
	var result SpeedResult
	if percent < 25 || percent > 1000 {
		return result, fmt.Errorf("percent must be in 25..1000")
	}
	err := c.callV2Params("speed", "speed_control", map[string]any{"percent": percent}, &result)
	return result, err
}

// RAMRead reads only the side-effect-free 64 KiB MD work RAM at a frame stop.
type RAMResult struct {
	Address uint32 `json:"address"`
	Size    uint32 `json:"size"`
	Frame   uint64 `json:"frame"`
	Hex     string `json:"hex"`
}

func (c *Client) ReadWorkRAM(address, size uint32) (RAMResult, error) {
	var result RAMResult
	if address < 0xFF0000 || address > 0xFFFFFF || size < 1 || size > 256 || size > 0x1000000-address {
		return result, fmt.Errorf("requires 1..256 bytes within FF0000..FFFFFF")
	}
	err := c.callV2Params("ram_read", "paused_work_ram", map[string]any{"address": address, "size": size}, &result)
	return result, err
}

// TraceWatch selects one value sampled at every frame boundary of a trace.
type TraceWatch struct {
	Space   string `json:"space"`
	Address uint32 `json:"address"`
	Size    uint32 `json:"size"`
}

// TraceUntil stops a trace at the first boundary where the masked watch matches.
type TraceUntil struct {
	Watch uint32  `json:"watch"`
	Op    string  `json:"op"`
	Value *uint32 `json:"value,omitempty"`
	Mask  *uint32 `json:"mask,omitempty"`
}

type TraceResult struct {
	FrameResult
	Records uint64 `json:"records"`
	Stop    string `json:"stop"`
}

// TraceContext runs a bounded frame trace; the core writes changed frames as JSONL to path.
func (c *Client) TraceContext(ctx context.Context, path string, frames uint32, watches []TraceWatch, until *TraceUntil) (TraceResult, error) {
	var result TraceResult
	if frames < 1 || frames > 36000 {
		return result, fmt.Errorf("frames must be in 1..36000")
	}
	if len(watches) < 1 || len(watches) > 16 {
		return result, fmt.Errorf("requires 1..16 watches")
	}
	list := make([]map[string]any, len(watches))
	for i, w := range watches {
		list[i] = map[string]any{"space": w.Space, "address": w.Address, "size": w.Size}
	}
	params := map[string]any{"frames": frames, "path": path, "watches": list}
	if until != nil {
		condition := map[string]any{"watch": until.Watch, "op": until.Op}
		if until.Value != nil {
			condition["value"] = *until.Value
		}
		if until.Mask != nil {
			condition["mask"] = *until.Mask
		}
		params["until"] = condition
	}
	err := c.callV2ParamsContext(ctx, "trace", "frame_trace", params, &result)
	return result, err
}

type VDPDumpResult struct {
	Frame  uint64 `json:"frame"`
	Paused bool   `json:"paused"`
	Format string `json:"format"`
}

// VDPDump writes a KITVDMP1 snapshot immediately while the emulator is stopped.
func (c *Client) VDPDump(path string) (VDPDumpResult, error) {
	var result VDPDumpResult
	err := c.callV2Params("vdp_dump", "paused_vdp_dump", map[string]any{"path": path}, &result)
	return result, err
}

type LineTraceResult struct {
	FrameResult
	Events uint64 `json:"events"`
}

// LineTraceContext runs 1..600 frames, logging VDP writes and interrupts with scanline positions to path.
func (c *Client) LineTraceContext(ctx context.Context, path string, frames uint32, kinds []string) (LineTraceResult, error) {
	var result LineTraceResult
	if frames < 1 || frames > 600 {
		return result, fmt.Errorf("frames must be in 1..600")
	}
	params := map[string]any{"frames": frames, "path": path}
	if len(kinds) > 0 {
		params["kinds"] = kinds
	}
	err := c.callV2ParamsContext(ctx, "line_trace", "line_trace", params, &result)
	return result, err
}

type Z80Cycles struct {
	Run    uint64 `json:"run" jsonschema:"Z80 cycles executed"`
	Busack uint64 `json:"busack" jsonschema:"Z80 cycles stopped because the 68000 held the bus"`
	Reset  uint64 `json:"reset" jsonschema:"Z80 cycles held in reset"`
}

// Z80State is the sound CPU at a stop; register pairs are 16-bit values. The JIT does not
// keep the Z80 PC current between sync points, so it is not reported.
type Z80State struct {
	Frame     uint64    `json:"frame"`
	SP        uint16    `json:"sp"`
	A         uint8     `json:"a"`
	F         uint8     `json:"f"`
	BC        uint16    `json:"bc"`
	DE        uint16    `json:"de"`
	HL        uint16    `json:"hl"`
	IX        uint16    `json:"ix"`
	IY        uint16    `json:"iy"`
	I         uint8     `json:"i"`
	IM        uint8     `json:"im"`
	IFF1      uint8     `json:"iff1"`
	IFF2      uint8     `json:"iff2"`
	Bank      uint16    `json:"bank" jsonschema:"68000 bank register for the Z80 8000-FFFF window"`
	Reset     bool      `json:"reset"`
	Busreq    bool      `json:"busreq"`
	Busack    bool      `json:"busack"`
	LastFrame Z80Cycles `json:"last_frame" jsonschema:"cycles during the last completed frame"`
	Total     Z80Cycles `json:"total"`
}

func (c *Client) Z80State() (Z80State, error) {
	var result Z80State
	err := c.callV2("z80_state", "z80_observation", &result)
	return result, err
}

func (c *Client) Z80Read(address, size uint32) (RAMResult, error) {
	var result RAMResult
	if address > 0x1FFF || size < 1 || size > 256 || address+size > 0x2000 {
		return result, fmt.Errorf("requires 1..256 bytes within Z80 RAM 0000..1FFF")
	}
	err := c.callV2Params("z80_read", "z80_observation", map[string]any{"address": address, "size": size}, &result)
	return result, err
}

type StateResult struct {
	Name       string `json:"name"`
	Bytes      uint64 `json:"bytes,omitempty"`
	Frame      uint64 `json:"frame"`
	SavedFrame uint64 `json:"saved_frame,omitempty"`
	Paused     bool   `json:"paused"`
}

// SaveState writes a named state for this ROM into the session state directory.
func (c *Client) SaveState(name string) (StateResult, error) {
	var result StateResult
	err := c.callV2Params("save_state", "state", map[string]any{"name": name}, &result)
	return result, err
}

// LoadState restores a named state saved from the same ROM; the session stays paused.
func (c *Client) LoadState(name string) (StateResult, error) {
	var result StateResult
	err := c.callV2Params("load_state", "state", map[string]any{"name": name}, &result)
	return result, err
}

type ResetResult struct {
	Kind      string `json:"kind"`
	Scheduled bool   `json:"scheduled"`
	Frame     uint64 `json:"frame"`
	Paused    bool   `json:"paused"`
}

// SoftReset asserts the reset line at the first cycle after the next resume.
func (c *Client) SoftReset() (ResetResult, error) {
	var result ResetResult
	err := c.callV2Params("reset", "reset", map[string]any{"kind": "soft"}, &result)
	return result, err
}
