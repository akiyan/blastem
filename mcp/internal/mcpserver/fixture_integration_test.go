package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFixtureMCPContract(t *testing.T) {
	binary := os.Getenv("BLASTEM_INTEGRATION_BINARY")
	rom := os.Getenv("BLASTEM_FIXTURE_ROM")
	symbols := os.Getenv("BLASTEM_FIXTURE_SYMBOLS")
	if binary == "" || rom == "" || symbols == "" {
		t.Skip("set BLASTEM_INTEGRATION_BINARY, BLASTEM_FIXTURE_ROM, and BLASTEM_FIXTURE_SYMBOLS to run")
	}
	breakpoint := fixtureSymbol(t, symbols, "fixture_breakpoint")
	t.Setenv("SDL_AUDIODRIVER", "dummy")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	app := New("fixture-integration-test")
	t.Cleanup(func() { _ = app.Close() })
	serverSession, err := app.Server().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "fixture-client", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	profile := t.TempDir()
	callOK(t, ctx, clientSession, "blastem_start", withHeadless(map[string]any{
		"binary_path": binary, "rom_path": rom, "debug": true, "save_dir": profile, "symbols_path": symbols,
	}))
	callError(t, ctx, clientSession, "paused_screenshot", map[string]any{}) // reset has no completed frame
	callError(t, ctx, clientSession, "frame_pause", map[string]any{})       // GDB owns CPU stop/resume
	set := callOK(t, ctx, clientSession, "breakpoint_set", map[string]any{"symbol": "fixture_breakpoint"}).StructuredContent.(map[string]any)
	if uint32(set["address"].(float64)) != breakpoint || set["location"] != "fixture_breakpoint" {
		t.Fatalf("symbol breakpoint = %+v, want %#x", set, breakpoint)
	}
	callError(t, ctx, clientSession, "breakpoint_set", map[string]any{"symbol": "missing_symbol"})
	callOK(t, ctx, clientSession, "cpu_continue", map[string]any{})
	if pc := callOK(t, ctx, clientSession, "cpu_registers", map[string]any{}).StructuredContent.(map[string]any)["pc_location"]; pc != "fixture_breakpoint" {
		t.Fatalf("pc_location = %v", pc)
	}

	markers := fixtureMemory(t, callOK(t, ctx, clientSession, "memory_read", map[string]any{
		"address": 0xFF0000, "size": 20,
	}))
	if got := string(markers[0:4]); got != "BMCP" {
		t.Fatalf("fixture magic = %q, want BMCP", got)
	}
	if got := markers[16:20]; string(got) != "\x11\x22\x33\x44" {
		t.Fatalf("fixture scratch = %X, want 11223344", got)
	}

	callOK(t, ctx, clientSession, "button_down", map[string]any{"pad": 1, "button": "a"})
	callOK(t, ctx, clientSession, "cpu_continue", map[string]any{})
	input := fixtureMemory(t, callOK(t, ctx, clientSession, "memory_read", map[string]any{
		"address": 0xFF0008, "size": 2,
	}))
	if input[1]&0x40 == 0 {
		t.Fatalf("fixture input state = %02X%02X, A bit is clear", input[0], input[1])
	}

	callOK(t, ctx, clientSession, "memory_write", map[string]any{"symbol": "fixture_scratch", "hex": "A5A55A5A"})
	scratch := fixtureMemory(t, callOK(t, ctx, clientSession, "memory_read", map[string]any{
		"address": 0xFF0010, "size": 4,
	}))
	if got := strings.ToUpper(jsonHex(scratch)); got != "A5A55A5A" {
		t.Fatalf("fixture scratch after write = %s", got)
	}

	// An unreachable breakpoint no longer hangs: the timeout interrupts the CPU (SIGINT).
	callOK(t, ctx, clientSession, "breakpoint_remove", map[string]any{"symbol": "fixture_breakpoint"})
	callOK(t, ctx, clientSession, "breakpoint_set", map[string]any{"symbol": "exception_handler"})
	stopped := callOK(t, ctx, clientSession, "cpu_continue", map[string]any{"timeout_ms": 300}).StructuredContent.(map[string]any)
	if stopped["interrupted"] != true || stopped["stop"].(map[string]any)["signal"].(float64) != 2 || stopped["pc_location"] == "" {
		t.Fatalf("interrupted continue = %+v", stopped)
	}
	// The control socket still answers while GDB holds the CPU at an instruction.
	held := callOK(t, ctx, clientSession, "ram_read", map[string]any{"symbol": "fixture_magic", "size": 4}).StructuredContent.(map[string]any)
	if held["hex"] != "424d4350" {
		t.Fatalf("ram_read during GDB stop = %+v", held)
	}
	callOK(t, ctx, clientSession, "breakpoint_remove", map[string]any{"symbol": "exception_handler"})
	callOK(t, ctx, clientSession, "breakpoint_set", map[string]any{"symbol": "fixture_breakpoint"})
	if again := callOK(t, ctx, clientSession, "cpu_continue", map[string]any{}).StructuredContent.(map[string]any); again["interrupted"] != false || again["pc_location"] != "fixture_breakpoint" {
		t.Fatalf("continue after interrupt = %+v", again)
	}
	// A write watchpoint stops right after the frame counter is incremented.
	callOK(t, ctx, clientSession, "breakpoint_remove", map[string]any{"symbol": "fixture_breakpoint"})
	callOK(t, ctx, clientSession, "watchpoint_set", map[string]any{"symbol": "fixture_frame_counter", "size": 4})
	watched := callOK(t, ctx, clientSession, "cpu_continue", map[string]any{"timeout_ms": 2000}).StructuredContent.(map[string]any)
	if reply := watched["stop"].(map[string]any)["reply"].(string); watched["interrupted"] != false || !strings.HasPrefix(reply, "T05watch:") {
		t.Fatalf("watchpoint stop = %+v", watched)
	}
	callOK(t, ctx, clientSession, "watchpoint_remove", map[string]any{"symbol": "fixture_frame_counter", "size": 4})
	callOK(t, ctx, clientSession, "breakpoint_set", map[string]any{"symbol": "fixture_breakpoint"})
	callOK(t, ctx, clientSession, "cpu_continue", map[string]any{})

	callOK(t, ctx, clientSession, "button_up", map[string]any{"pad": 1, "button": "a"})
	callOK(t, ctx, clientSession, "cpu_continue", map[string]any{})
	input = fixtureMemory(t, callOK(t, ctx, clientSession, "memory_read", map[string]any{
		"address": 0xFF0008, "size": 2,
	}))
	if input[1]&0x40 != 0 {
		t.Fatalf("fixture input state = %02X%02X, A bit remains set after release", input[0], input[1])
	}
	// Both captures must observe the same complete frame without CPU progress.
	registersBefore := callOK(t, ctx, clientSession, "cpu_registers", map[string]any{}).StructuredContent
	shotA := callOK(t, ctx, clientSession, "paused_screenshot", map[string]any{})
	shotB := callOK(t, ctx, clientSession, "paused_screenshot", map[string]any{})
	registersAfter := callOK(t, ctx, clientSession, "cpu_registers", map[string]any{}).StructuredContent
	if !reflect.DeepEqual(registersBefore, registersAfter) {
		t.Fatal("paused screenshot advanced CPU")
	}
	imageA := shotA.Content[0].(*mcp.ImageContent).Data
	imageB := shotB.Content[0].(*mcp.ImageContent).Data
	if !bytes.Equal(imageA, imageB) {
		t.Fatal("paused screenshot changed pixels")
	}
	decoded, err := png.Decode(bytes.NewReader(imageA))
	if err != nil || decoded.Bounds().Dx() < 256 || decoded.Bounds().Dy() < 200 {
		t.Fatalf("invalid cached frame: %v", err)
	}
	var a, b pausedScreenshotOutput
	for _, item := range []struct {
		src any
		dst *pausedScreenshotOutput
	}{{shotA.StructuredContent, &a}, {shotB.StructuredContent, &b}} {
		raw, err := json.Marshal(item.src)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, item.dst); err != nil {
			t.Fatal(err)
		}
	}
	if a.Snapshot != b.Snapshot || !a.Snapshot.Paused || a.Snapshot.Sequence == 0 {
		t.Fatalf("unstable frame metadata: %+v %+v", a, b)
	}
	callOK(t, ctx, clientSession, "breakpoint_remove", map[string]any{"address": breakpoint})
	callOK(t, ctx, clientSession, "save_flush", map[string]any{})
	saved, err := os.ReadFile(filepath.Join(profile, "save.sram"))
	if err != nil || len(saved) != 32768 || saved[0] != 0xA5 {
		t.Fatalf("MCP save flush: length=%d error=%v", len(saved), err)
	}
	callOK(t, ctx, clientSession, "blastem_stop", map[string]any{})
}

func fixtureSymbol(t *testing.T, path, name string) uint32 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 3 && fields[2] == name {
			value, err := strconv.ParseUint(fields[0], 16, 32)
			if err != nil {
				t.Fatal(err)
			}
			return uint32(value)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("symbol %q not found in %s", name, path)
	return 0
}

func fixtureMemory(t *testing.T, result *mcp.CallToolResult) []byte {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output memoryReadOutput
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, output.Size)
	for i := range data {
		value, err := strconv.ParseUint(output.Hex[i*2:i*2+2], 16, 8)
		if err != nil {
			t.Fatal(err)
		}
		data[i] = byte(value)
	}
	return data
}

func jsonHex(data []byte) string {
	const digits = "0123456789ABCDEF"
	result := make([]byte, len(data)*2)
	for i, value := range data {
		result[i*2] = digits[value>>4]
		result[i*2+1] = digits[value&15]
	}
	return string(result)
}
