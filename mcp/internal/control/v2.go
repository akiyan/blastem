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
	if command == "advance" {
		return v.requestReady(command, params, out, 60*time.Second, func() {
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
