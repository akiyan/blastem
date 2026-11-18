package mcpserver

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akiyan/blastem/mcp/internal/artifact"
	"github.com/akiyan/blastem/mcp/internal/control"
	"github.com/akiyan/blastem/mcp/internal/gdbrsp"
	"github.com/akiyan/blastem/mcp/internal/session"
	"github.com/akiyan/blastem/mcp/internal/symbols"
	"github.com/akiyan/blastem/mcp/internal/vdp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type App struct {
	server    *mcp.Server
	session   *session.Manager
	serial    atomic.Uint64
	captureMu sync.Mutex
	symbolsMu sync.Mutex
	symbols   *symbols.Table
}

func (a *App) symbolTable() *symbols.Table {
	a.symbolsMu.Lock()
	defer a.symbolsMu.Unlock()
	return a.symbols
}

// address resolves an optional symbol expression, falling back to the numeric address.
func (a *App) address(symbol string, address uint32) (uint32, error) {
	if symbol == "" {
		return address, nil
	}
	resolved, err := a.symbolTable().Resolve(symbol)
	if err == nil && address != 0 && address != resolved {
		err = fmt.Errorf("address %#x conflicts with symbol %q (%#x)", address, symbol, resolved)
	}
	return resolved, err
}

func New(version string) *App {
	a := &App{session: session.NewManager()}
	a.server = mcp.NewServer(&mcp.Implementation{Name: "blastem-mcp", Version: version}, &mcp.ServerOptions{Instructions: instructions})
	a.registerTools()
	return a
}

func (a *App) Server() *mcp.Server { return a.server }
func (a *App) Close() error        { return a.session.Close() }

// instructions is sent to MCP clients at initialization; mcp/AGENT_GUIDE.md has the full guide.
const instructions = `BlastEM (Mega Drive/Genesis) control and observation. Verify with text/numbers rather than images:
1. blastem_start (use headless:true without a display; symbols_path enables symbol names), then frame_pause.
2. Act from the pause: frame_advance, button_press, input_sequence (frame-offset input list), reset, state_save/state_load.
3. Observe without advancing: ram_read, frame_trace (per-frame values, stop on a condition), vdp_layout (planes/sprites as text),
   line_trace (per-scanline VDP writes and interrupt cycles), z80_state. Use paused_screenshot only when an image is needed.
Same state + same input gives the same result. debug:true (GDB) adds registers, memory, breakpoints, write watchpoints,
step and cpu_continue (timeout_ms), but cannot use frame tools. The control socket and GDB can also be used directly (see guide).
Call blastem_stop at the end. Full guide: mcp/AGENT_GUIDE.md in https://github.com/akiyan/blastem (akiyan branch).`

type emptyInput struct{}

type startInput struct {
	SaveDir    string `json:"save_dir,omitempty" jsonschema:"absolute persistent battery-save directory; requires patched core; exclusive to this session"`
	BinaryPath string `json:"binary_path" jsonschema:"absolute or working-directory-relative path to the BlastEM executable"`
	ROMPath    string `json:"rom_path" jsonschema:"absolute or working-directory-relative path to a local ROM"`
	Debug      bool   `json:"debug,omitempty" jsonschema:"start the localhost GDB Remote stub"`
	Headless   bool   `json:"headless,omitempty" jsonschema:"run without a display server (SDL dummy video/audio, software renderer); no Xvfb needed"`
	StateDir   string `json:"state_dir,omitempty" jsonschema:"absolute directory for named save states; default a session-private directory removed at stop"`
	Symbols    string `json:"symbols_path,omitempty" jsonschema:"optional nm -n style symbol file (ADDRESS TYPE NAME); enables symbol names in memory, RAM, breakpoint, and trace tools"`
}

type statusOutput struct {
	Session session.Status `json:"session"`
}

type inputArgs struct {
	Pad    int    `json:"pad" jsonschema:"controller port number, 1 or 2"`
	Button string `json:"button" jsonschema:"button: up, down, left, right, a, b, c, x, y, z, start, or mode"`
}

type operationOutput struct {
	OK bool `json:"ok"`
}

type artifactOutput struct {
	Artifact artifact.Info `json:"artifact"`
}

type vdpSnapshotOutput struct {
	Artifact artifact.Info    `json:"artifact"`
	Snapshot vdp.SnapshotInfo `json:"snapshot"`
}

type vramTilesInput struct {
	Scale int `json:"scale,omitempty" jsonschema:"pixel scale from 1 through 8; defaults to 2"`
}

type vramTilesOutput struct {
	ReferenceFrame artifact.Info       `json:"reference_frame"`
	Palettes       []vramPaletteOutput `json:"palettes"`
}

type vramPaletteOutput struct {
	Palette  int               `json:"palette"`
	Artifact artifact.Info     `json:"artifact"`
	Tiles    vdp.TilesheetInfo `json:"tiles"`
}

type memoryReadInput struct {
	Address uint32 `json:"address,omitempty" jsonschema:"68000 address (or use symbol)"`
	Symbol  string `json:"symbol,omitempty" jsonschema:"symbol name, optionally NAME+OFFSET; requires symbols_path"`
	Size    int    `json:"size" jsonschema:"number of bytes from 0 through 65536"`
}

type memoryReadOutput struct {
	Address  uint32 `json:"address"`
	Location string `json:"location,omitempty" jsonschema:"nearest symbol at or below the address"`
	Size     int    `json:"size"`
	Hex      string `json:"hex" jsonschema:"uppercase hexadecimal bytes"`
}

type memoryWriteInput struct {
	Address uint32 `json:"address,omitempty" jsonschema:"68000 address (or use symbol)"`
	Symbol  string `json:"symbol,omitempty" jsonschema:"symbol name, optionally NAME+OFFSET; requires symbols_path"`
	Hex     string `json:"hex" jsonschema:"hexadecimal bytes, at most 65536 decoded bytes"`
}

type breakpointInput struct {
	Address uint32 `json:"address,omitempty" jsonschema:"24-bit 68000 breakpoint address (or use symbol)"`
	Symbol  string `json:"symbol,omitempty" jsonschema:"symbol name, optionally NAME+OFFSET; requires symbols_path"`
}

type watchpointInput struct {
	Address uint32 `json:"address,omitempty" jsonschema:"24-bit 68000 address (or use symbol)"`
	Symbol  string `json:"symbol,omitempty" jsonschema:"symbol name, optionally NAME+OFFSET; requires symbols_path"`
	Size    uint32 `json:"size,omitempty" jsonschema:"bytes covered, 1..65536; default 1"`
}

func (a *App) watchpointSet(ctx context.Context, _ *mcp.CallToolRequest, in watchpointInput) (*mcp.CallToolResult, breakpointOutput, error) {
	return a.watchpoint(ctx, in, true)
}

func (a *App) watchpointRemove(ctx context.Context, _ *mcp.CallToolRequest, in watchpointInput) (*mcp.CallToolResult, breakpointOutput, error) {
	return a.watchpoint(ctx, in, false)
}

func (a *App) watchpoint(ctx context.Context, in watchpointInput, set bool) (*mcp.CallToolResult, breakpointOutput, error) {
	address, err := a.address(in.Symbol, in.Address)
	if err != nil {
		return nil, breakpointOutput{}, err
	}
	size := in.Size
	if size == 0 {
		size = 1
	}
	client, err := a.session.GDB()
	if err == nil {
		if set {
			err = client.SetWatchpoint(ctx, address, size)
		} else {
			err = client.RemoveWatchpoint(ctx, address, size)
		}
	}
	return nil, breakpointOutput{OK: err == nil, Address: address, Location: a.symbolTable().Locate(address)}, err
}

type breakpointOutput struct {
	OK       bool   `json:"ok"`
	Address  uint32 `json:"address"`
	Location string `json:"location,omitempty"`
}

type registersOutput struct {
	Registers gdbrsp.Registers `json:"registers"`
	Location  string           `json:"pc_location,omitempty" jsonschema:"PC as nearest symbol+offset when symbols are loaded"`
}

type stopOutput struct {
	Stop gdbrsp.Stop `json:"stop"`
}

func (a *App) registerTools() {
	mcp.AddTool(a.server, &mcp.Tool{Name: "audio_capture", Description: "Pause a non-GDB MD session, advance 1..600 frames, and return bounded stereo WAV from the host mixer. Mixer latency applies; not a sample-exact frame interval."}, a.audioCapture)
	mcp.AddTool(a.server, &mcp.Tool{Name: "ram_read", Description: "Read 1..256 bytes of MD work RAM (FF0000..FFFFFF) at a paused frame boundary, without GDB or CPU side effects."}, a.ramRead)
	mcp.AddTool(a.server, &mcp.Tool{Name: "speed_set", Description: "Set 25..1000 percent execution speed while paused in a non-GDB MD session. Changes host pacing/audio; frame counts remain unchanged."}, a.speedSet)
	mcp.AddTool(a.server, &mcp.Tool{Name: "frame_pause", Description: "Pause a non-GDB Genesis session at the next frame synchronization point."}, a.framePause)
	mcp.AddTool(a.server, &mcp.Tool{Name: "frame_advance", Description: "Advance 1..600 frames from a frame pause, then stop. Non-GDB Genesis only."}, a.frameAdvance)
	mcp.AddTool(a.server, &mcp.Tool{Name: "frame_trace", Description: "Run up to 36000 frames from a frame pause, sampling 1..16 RAM/CRAM/VSRAM/VDP-register/VRAM/Z80-RAM values at every frame boundary. Returns only frames where a value changed (plus the start), optionally stopping when a condition matches. Non-GDB Genesis only; frame_interrupt stops it."}, a.frameTrace)
	mcp.AddTool(a.server, &mcp.Tool{Name: "vdp_layout", Description: "While stopped, decode the display as text: mode, scroll runs, plane A/B/window name-table grids for the visible screen cells (TILE/PALETTE, * priority, h/v flips), and the linked sprite list. No image is rendered."}, a.vdpLayout)
	mcp.AddTool(a.server, &mcp.Tool{Name: "line_trace", Description: "Run 1..600 frames from a frame pause and log CRAM/VSRAM/VDP-register writes and interrupt acknowledges with the VDP line and slot where each took effect, plus 68000 cycles from each interrupt to its RTE. Returns a per-level interrupt summary and the events. Non-GDB Genesis only."}, a.lineTrace)
	mcp.AddTool(a.server, &mcp.Tool{Name: "z80_state", Description: "While stopped, read the sound CPU: Z80 registers, reset/BUSREQ state, cycles executed or stopped in the last frame and in total, and optionally 1..256 bytes of Z80 RAM."}, a.z80State)
	mcp.AddTool(a.server, &mcp.Tool{Name: "symbol_lookup", Description: "Search the symbol file loaded by blastem_start (symbols_path) by name substring, or name an address as the nearest symbol+offset."}, a.symbolLookup)
	mcp.AddTool(a.server, &mcp.Tool{Name: "state_save", Description: "While frame-paused, save the whole machine state under a name in the session state directory. Bound to this ROM; the session stays paused."}, a.stateSave)
	mcp.AddTool(a.server, &mcp.Tool{Name: "state_load", Description: "While frame-paused, restore a named state saved from the same ROM and stay paused. The cached screenshot is stale until the next frame advance."}, a.stateLoad)
	mcp.AddTool(a.server, &mcp.Tool{Name: "state_list", Description: "List named save states in the session state directory."}, a.stateList)
	mcp.AddTool(a.server, &mcp.Tool{Name: "reset", Description: "While frame-paused, release held buttons and schedule a deterministic soft reset (reset button) at the first cycle of the next advance."}, a.reset)
	mcp.AddTool(a.server, &mcp.Tool{Name: "button_press", Description: "From a frame pause, hold a button for exactly N frames (1..600), then release it at the final frame boundary."}, a.buttonPress)
	mcp.AddTool(a.server, &mcp.Tool{Name: "input_sequence", Description: "From a frame pause, run up to 36000 frames applying button down/up events at given frame offsets in one call; releases held buttons at the end unless keep_held."}, a.inputSequence)
	mcp.AddTool(a.server, &mcp.Tool{Name: "watchpoint_set", Description: "Debug session: stop the 68000 after any write to address..address+size-1 (write watchpoints only). cpu_continue then reports stop reply T05watch:ADDR."}, a.watchpointSet)
	mcp.AddTool(a.server, &mcp.Tool{Name: "watchpoint_remove", Description: "Debug session: remove a write watchpoint set with the same address and size."}, a.watchpointRemove)
	mcp.AddTool(a.server, &mcp.Tool{Name: "frame_interrupt", Description: "Request interruption of an active frame_advance or frame_trace; its response reports the final stopped state."}, a.frameInterrupt)
	mcp.AddTool(a.server, &mcp.Tool{Name: "paused_screenshot", Description: "Return the last completed progressive frame without advancing the stopped CPU. Requires cached_screenshot core capability; fails if running or no frame exists."}, a.pausedScreenshot)
	mcp.AddTool(a.server, &mcp.Tool{Name: "blastem_start", Description: "Start one managed BlastEM session with a local ROM."}, a.start)
	mcp.AddTool(a.server, &mcp.Tool{Name: "save_flush", Description: "Atomically persist cartridge SRAM and return the completed write result."}, a.saveFlush)
	mcp.AddTool(a.server, &mcp.Tool{Name: "blastem_status", Description: "Inspect the managed BlastEM process and endpoints."}, a.status)
	mcp.AddTool(a.server, &mcp.Tool{Name: "blastem_stop", Description: "Release held input and stop the managed BlastEM process."}, a.stop)
	mcp.AddTool(a.server, &mcp.Tool{Name: "button_down", Description: "Hold a Mega Drive controller button through BlastEM's control socket."}, a.buttonDown)
	mcp.AddTool(a.server, &mcp.Tool{Name: "button_up", Description: "Release a Mega Drive controller button through BlastEM's control socket."}, a.buttonUp)
	mcp.AddTool(a.server, &mcp.Tool{Name: "release_all_buttons", Description: "Release every controller button held through this MCP session."}, a.releaseAll)
	mcp.AddTool(a.server, &mcp.Tool{Name: "screenshot", Description: "Capture the next BlastEM-rendered frame as a PNG image."}, a.screenshot)
	mcp.AddTool(a.server, &mcp.Tool{Name: "vdp_snapshot", Description: "Capture a KITVDMP1 VRAM/CRAM/VSRAM/VDP-register snapshot."}, a.vdpSnapshot)
	mcp.AddTool(a.server, &mcp.Tool{Name: "vram_tiles", Description: "Capture VRAM and return four PNG tilesheets, one for each live CRAM palette in palette 0 through 3 order."}, a.vramTiles)
	mcp.AddTool(a.server, &mcp.Tool{Name: "cpu_registers", Description: "Read all 68000 data/address registers, status register, and PC while stopped."}, a.cpuRegisters)
	mcp.AddTool(a.server, &mcp.Tool{Name: "memory_read", Description: "Read up to 64 KiB from the 68000 address space while stopped."}, a.memoryRead)
	mcp.AddTool(a.server, &mcp.Tool{Name: "memory_write", Description: "Write hexadecimal bytes into the 68000 address space while stopped."}, a.memoryWrite)
	mcp.AddTool(a.server, &mcp.Tool{Name: "breakpoint_set", Description: "Set a 68000 software breakpoint at a 24-bit address."}, a.breakpointSet)
	mcp.AddTool(a.server, &mcp.Tool{Name: "breakpoint_remove", Description: "Remove a 68000 software breakpoint."}, a.breakpointRemove)
	mcp.AddTool(a.server, &mcp.Tool{Name: "cpu_step", Description: "Execute one 68000 instruction and wait for the stop reply."}, a.cpuStep)
	mcp.AddTool(a.server, &mcp.Tool{Name: "cpu_continue", Description: "Continue 68000 execution until a breakpoint stops it, or interrupt it (SIGINT) after timeout_ms (default 10000) and report the stop location."}, a.cpuContinue)
}

func (a *App) start(_ context.Context, _ *mcp.CallToolRequest, in startInput) (*mcp.CallToolResult, statusOutput, error) {
	var table *symbols.Table
	if in.Symbols != "" {
		loaded, err := symbols.Load(in.Symbols)
		if err != nil {
			return nil, statusOutput{}, fmt.Errorf("load symbols: %w", err)
		}
		table = loaded
	}
	status, err := a.session.Start(session.StartConfig{BinaryPath: in.BinaryPath, ROMPath: in.ROMPath, Debug: in.Debug, SaveDir: in.SaveDir, Headless: in.Headless, StateDir: in.StateDir})
	if err == nil {
		a.symbolsMu.Lock()
		a.symbols = table
		a.symbolsMu.Unlock()
	}
	return nil, statusOutput{Session: status}, err
}

func (a *App) status(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, statusOutput, error) {
	return nil, statusOutput{Session: a.session.Status()}, nil
}

func (a *App) stop(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, operationOutput, error) {
	err := a.session.Stop()
	return nil, operationOutput{OK: err == nil}, err
}

func (a *App) buttonDown(_ context.Context, _ *mcp.CallToolRequest, in inputArgs) (*mcp.CallToolResult, operationOutput, error) {
	client, _, err := a.session.Control()
	if err == nil {
		err = client.ButtonDown(in.Pad, in.Button)
	}
	return nil, operationOutput{OK: err == nil}, err
}

func (a *App) buttonUp(_ context.Context, _ *mcp.CallToolRequest, in inputArgs) (*mcp.CallToolResult, operationOutput, error) {
	client, _, err := a.session.Control()
	if err == nil {
		err = client.ButtonUp(in.Pad, in.Button)
	}
	return nil, operationOutput{OK: err == nil}, err
}

func (a *App) releaseAll(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, operationOutput, error) {
	client, _, err := a.session.Control()
	if err == nil {
		err = client.ReleaseAll()
	}
	return nil, operationOutput{OK: err == nil}, err
}

func (a *App) screenshot(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, artifactOutput, error) {
	client, runtimeDir, err := a.session.Control()
	if err != nil {
		return nil, artifactOutput{}, err
	}
	path := filepath.Join(runtimeDir, fmt.Sprintf("screenshot-%06d.png", a.serial.Add(1)))
	if err := client.Screenshot(path); err != nil {
		return nil, artifactOutput{}, err
	}
	info, err := artifact.Inspect(path)
	if err != nil {
		return nil, artifactOutput{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, artifactOutput{}, err
	}
	result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{
		Data: data, MIMEType: "image/png",
	}}}
	return result, artifactOutput{Artifact: info}, nil
}

func (a *App) vdpSnapshot(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, vdpSnapshotOutput, error) {
	client, runtimeDir, err := a.session.Control()
	if err != nil {
		return nil, vdpSnapshotOutput{}, err
	}
	path := filepath.Join(runtimeDir, fmt.Sprintf("vdp-%06d.kitvdmp", a.serial.Add(1)))
	if err := client.VDPSnapshot(path); err != nil {
		return nil, vdpSnapshotOutput{}, err
	}
	info, err := artifact.Inspect(path)
	if err != nil {
		return nil, vdpSnapshotOutput{}, err
	}
	snapshot, err := vdp.Read(path)
	return nil, vdpSnapshotOutput{Artifact: info, Snapshot: snapshot.Info()}, err
}

func (a *App) vramTiles(_ context.Context, _ *mcp.CallToolRequest, in vramTilesInput) (*mcp.CallToolResult, vramTilesOutput, error) {
	client, runtimeDir, err := a.session.Control()
	if err != nil {
		return nil, vramTilesOutput{}, err
	}
	scale := in.Scale
	if scale == 0 {
		scale = 2
	}
	serial := a.serial.Add(1)
	snapshotPath := filepath.Join(runtimeDir, fmt.Sprintf("vdp-%06d.kitvdmp", serial))
	referencePath := filepath.Join(runtimeDir, fmt.Sprintf("vram-reference-%06d.png", serial))
	if err := client.VideoState(referencePath, snapshotPath); err != nil {
		return nil, vramTilesOutput{}, err
	}
	referenceInfo, err := artifact.Inspect(referencePath)
	if err != nil {
		return nil, vramTilesOutput{}, err
	}
	snapshot, err := vdp.Read(snapshotPath)
	if err != nil {
		return nil, vramTilesOutput{}, err
	}
	output := vramTilesOutput{ReferenceFrame: referenceInfo, Palettes: make([]vramPaletteOutput, 0, 4)}
	content := make([]mcp.Content, 0, 4)
	for palette := 0; palette < 4; palette++ {
		imagePath := filepath.Join(runtimeDir, fmt.Sprintf("vram-palette-%d-%06d.png", palette, serial))
		tiles, err := snapshot.WritePaletteTilesheet(imagePath, scale, palette)
		if err != nil {
			return nil, vramTilesOutput{}, err
		}
		info, err := artifact.Inspect(imagePath)
		if err != nil {
			return nil, vramTilesOutput{}, err
		}
		data, err := os.ReadFile(imagePath)
		if err != nil {
			return nil, vramTilesOutput{}, err
		}
		output.Palettes = append(output.Palettes, vramPaletteOutput{Palette: palette, Artifact: info, Tiles: tiles})
		content = append(content, &mcp.ImageContent{Data: data, MIMEType: "image/png"})
	}
	return &mcp.CallToolResult{Content: content}, output, nil
}

func (a *App) cpuRegisters(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, registersOutput, error) {
	client, err := a.session.GDB()
	if err != nil {
		return nil, registersOutput{}, err
	}
	registers, err := client.Registers(ctx)
	return nil, registersOutput{Registers: registers, Location: a.symbolTable().Locate(registers.PC)}, err
}

func (a *App) memoryRead(ctx context.Context, _ *mcp.CallToolRequest, in memoryReadInput) (*mcp.CallToolResult, memoryReadOutput, error) {
	client, err := a.session.GDB()
	if err != nil {
		return nil, memoryReadOutput{}, err
	}
	address, err := a.address(in.Symbol, in.Address)
	if err != nil {
		return nil, memoryReadOutput{}, err
	}
	data, err := client.ReadMemory(ctx, address, in.Size)
	return nil, memoryReadOutput{Address: address, Location: a.symbolTable().Locate(address), Size: len(data), Hex: strings.ToUpper(hex.EncodeToString(data))}, err
}

func (a *App) memoryWrite(ctx context.Context, _ *mcp.CallToolRequest, in memoryWriteInput) (*mcp.CallToolResult, operationOutput, error) {
	client, err := a.session.GDB()
	if err != nil {
		return nil, operationOutput{}, err
	}
	address, err := a.address(in.Symbol, in.Address)
	if err != nil {
		return nil, operationOutput{}, err
	}
	data, err := hex.DecodeString(in.Hex)
	if err == nil {
		err = client.WriteMemory(ctx, address, data)
	}
	return nil, operationOutput{OK: err == nil}, err
}

func (a *App) breakpointSet(ctx context.Context, _ *mcp.CallToolRequest, in breakpointInput) (*mcp.CallToolResult, breakpointOutput, error) {
	address, err := a.address(in.Symbol, in.Address)
	if err != nil {
		return nil, breakpointOutput{}, err
	}
	client, err := a.session.GDB()
	if err == nil {
		err = client.SetBreakpoint(ctx, address)
	}
	return nil, breakpointOutput{OK: err == nil, Address: address, Location: a.symbolTable().Locate(address)}, err
}

func (a *App) breakpointRemove(ctx context.Context, _ *mcp.CallToolRequest, in breakpointInput) (*mcp.CallToolResult, breakpointOutput, error) {
	address, err := a.address(in.Symbol, in.Address)
	if err != nil {
		return nil, breakpointOutput{}, err
	}
	client, err := a.session.GDB()
	if err == nil {
		err = client.RemoveBreakpoint(ctx, address)
	}
	return nil, breakpointOutput{OK: err == nil, Address: address, Location: a.symbolTable().Locate(address)}, err
}

func (a *App) cpuStep(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, stopOutput, error) {
	client, err := a.session.GDB()
	if err != nil {
		return nil, stopOutput{}, err
	}
	stop, err := client.Step(ctx)
	return nil, stopOutput{Stop: stop}, err
}

type continueInput struct {
	TimeoutMS uint32 `json:"timeout_ms,omitempty" jsonschema:"stop the CPU with an interrupt if no breakpoint is reached within this many milliseconds; default 10000, at most 600000"`
}

type continueOutput struct {
	Stop        gdbrsp.Stop `json:"stop"`
	Interrupted bool        `json:"interrupted" jsonschema:"true when the timeout stopped the CPU (SIGINT) instead of a breakpoint"`
	PC          uint32      `json:"pc"`
	Location    string      `json:"pc_location,omitempty"`
}

func (a *App) cpuContinue(ctx context.Context, _ *mcp.CallToolRequest, in continueInput) (*mcp.CallToolResult, continueOutput, error) {
	timeout := in.TimeoutMS
	if timeout == 0 {
		timeout = 10000
	}
	if timeout > 600000 {
		return nil, continueOutput{}, fmt.Errorf("timeout_ms must be at most 600000")
	}
	client, err := a.session.GDB()
	if err != nil {
		return nil, continueOutput{}, err
	}
	stop, interrupted, err := client.ContinueFor(ctx, time.Duration(timeout)*time.Millisecond)
	out := continueOutput{Stop: stop, Interrupted: interrupted}
	if err == nil {
		if registers, regErr := client.Registers(ctx); regErr == nil {
			out.PC, out.Location = registers.PC, a.symbolTable().Locate(registers.PC)
		}
	}
	return nil, out, err
}

func (a *App) saveFlush(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, control.SaveResult, error) {
	client, _, err := a.session.Control()
	if err != nil {
		return nil, control.SaveResult{}, err
	}
	result, err := client.FlushSRAM()
	return nil, result, err
}

type pausedScreenshotOutput struct {
	Artifact artifact.Info          `json:"artifact"`
	Snapshot control.SnapshotResult `json:"snapshot"`
}

func (a *App) pausedScreenshot(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, pausedScreenshotOutput, error) {
	client, runtimeDir, err := a.session.Control()
	if err != nil {
		return nil, pausedScreenshotOutput{}, err
	}
	path := filepath.Join(runtimeDir, fmt.Sprintf("paused-%06d.png", a.serial.Add(1)))
	snapshot, err := client.Snapshot(path)
	if err != nil {
		return nil, pausedScreenshotOutput{}, err
	}
	info, err := artifact.Inspect(path)
	if err != nil {
		return nil, pausedScreenshotOutput{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, pausedScreenshotOutput{}, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: data, MIMEType: "image/png"}}}, pausedScreenshotOutput{Artifact: info, Snapshot: snapshot}, nil
}

type frameAdvanceInput struct {
	Frames uint32 `json:"frames" jsonschema:"number of frames, 1 through 600"`
}

func (a *App) framePause(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, control.FrameResult, error) {
	client, _, err := a.session.Control()
	if err != nil {
		return nil, control.FrameResult{}, err
	}
	result, err := client.Pause()
	return nil, result, err
}

func (a *App) frameAdvance(ctx context.Context, _ *mcp.CallToolRequest, in frameAdvanceInput) (*mcp.CallToolResult, control.FrameResult, error) {
	client, _, err := a.session.Control()
	if err != nil {
		return nil, control.FrameResult{}, err
	}
	result, err := client.AdvanceContext(ctx, in.Frames)
	return nil, result, err
}

func (a *App) frameInterrupt(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, operationOutput, error) {
	client, _, err := a.session.Control()
	if err == nil {
		err = client.Interrupt()
	}
	return nil, operationOutput{OK: err == nil}, err
}

type audioCaptureOutput struct {
	Artifact artifact.Info       `json:"artifact"`
	Audio    control.AudioResult `json:"audio"`
	Frames   control.FrameResult `json:"frames"`
}

func (a *App) audioCapture(ctx context.Context, _ *mcp.CallToolRequest, in frameAdvanceInput) (*mcp.CallToolResult, audioCaptureOutput, error) {
	a.captureMu.Lock()
	defer a.captureMu.Unlock()
	if in.Frames < 1 || in.Frames > 600 {
		return nil, audioCaptureOutput{}, fmt.Errorf("frames must be in 1..600")
	}
	client, dir, err := a.session.Control()
	if err != nil {
		return nil, audioCaptureOutput{}, err
	}
	if _, err := client.Pause(); err != nil {
		return nil, audioCaptureOutput{}, err
	}
	path := filepath.Join(dir, fmt.Sprintf("audio-%06d.wav", a.serial.Add(1)))
	if _, err := client.StartAudio(path, 30); err != nil {
		return nil, audioCaptureOutput{}, err
	}
	frames, runErr := client.AdvanceContext(ctx, in.Frames)
	audio, stopErr := client.StopAudio()
	if err := errors.Join(runErr, stopErr); err != nil {
		return nil, audioCaptureOutput{}, err
	}
	info, err := artifact.Inspect(path)
	if err != nil {
		return nil, audioCaptureOutput{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, audioCaptureOutput{}, err
	}
	result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.AudioContent{Data: data, MIMEType: "audio/wav"}}}
	return result, audioCaptureOutput{Artifact: info, Audio: audio, Frames: frames}, nil
}

type speedInput struct {
	Percent uint32 `json:"percent" jsonschema:"execution speed percentage, 25..1000"`
}

func (a *App) speedSet(_ context.Context, _ *mcp.CallToolRequest, in speedInput) (*mcp.CallToolResult, control.SpeedResult, error) {
	client, _, err := a.session.Control()
	if err != nil {
		return nil, control.SpeedResult{}, err
	}
	result, err := client.SetSpeed(in.Percent)
	return nil, result, err
}

type ramReadInput struct {
	Address uint32 `json:"address,omitempty" jsonschema:"work RAM address FF0000..FFFFFF (or use symbol)"`
	Symbol  string `json:"symbol,omitempty" jsonschema:"symbol name, optionally NAME+OFFSET; requires symbols_path"`
	Size    uint32 `json:"size"`
}

type ramReadOutput struct {
	control.RAMResult
	Location string `json:"location,omitempty" jsonschema:"nearest symbol at or below the address"`
}

func (a *App) ramRead(_ context.Context, _ *mcp.CallToolRequest, in ramReadInput) (*mcp.CallToolResult, ramReadOutput, error) {
	address, err := a.address(in.Symbol, in.Address)
	if err != nil {
		return nil, ramReadOutput{}, err
	}
	client, _, err := a.session.Control()
	if err != nil {
		return nil, ramReadOutput{}, err
	}
	result, err := client.ReadWorkRAM(address, in.Size)
	return nil, ramReadOutput{RAMResult: result, Location: a.symbolTable().Locate(address)}, err
}

type symbolLookupInput struct {
	Name    string  `json:"name,omitempty" jsonschema:"substring to search for in symbol names"`
	Address *uint32 `json:"address,omitempty" jsonschema:"address to name as the nearest symbol at or below it"`
	Limit   int     `json:"limit,omitempty" jsonschema:"maximum matches for name, default 50"`
}

type symbolLookupOutput struct {
	Path     string           `json:"path"`
	Count    int              `json:"count" jsonschema:"symbols in the loaded file"`
	Matches  []symbols.Symbol `json:"matches"`
	Location string           `json:"location,omitempty"`
}

func (a *App) symbolLookup(_ context.Context, _ *mcp.CallToolRequest, in symbolLookupInput) (*mcp.CallToolResult, symbolLookupOutput, error) {
	table := a.symbolTable()
	if table == nil {
		return nil, symbolLookupOutput{}, fmt.Errorf("no symbol file loaded; pass symbols_path to blastem_start")
	}
	limit := in.Limit
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 1000 {
		return nil, symbolLookupOutput{}, fmt.Errorf("limit must be in 1..1000")
	}
	out := symbolLookupOutput{Path: table.Path, Count: table.Len(), Matches: []symbols.Symbol{}}
	if in.Name != "" {
		out.Matches = table.Find(in.Name, limit)
	}
	if in.Address != nil {
		out.Location = table.Locate(*in.Address)
	}
	return nil, out, nil
}
