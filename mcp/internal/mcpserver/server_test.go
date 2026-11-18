package mcpserver

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAdvertisesImplementedTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	app := New("test")
	t.Cleanup(func() { _ = app.Close() })
	serverSession, err := app.Server().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	if init := clientSession.InitializeResult(); init == nil || !strings.Contains(init.Instructions, "frame_pause") {
		t.Fatalf("missing server instructions: %+v", init)
	}

	result, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		got = append(got, tool.Name)
	}
	sort.Strings(got)
	want := []string{
		"audio_capture", "blastem_start", "blastem_status", "blastem_stop", "breakpoint_remove", "breakpoint_set",
		"button_down", "button_press", "button_up", "cpu_continue", "cpu_registers", "cpu_step", "frame_advance",
		"frame_interrupt", "frame_pause", "frame_trace", "input_sequence", "line_trace", "memory_read",
		"memory_write", "paused_screenshot", "ram_read", "release_all_buttons", "reset", "save_flush",
		"screenshot", "speed_set", "state_list", "state_load", "state_save", "symbol_lookup", "vdp_layout",
		"vdp_snapshot", "vram_tiles", "watchpoint_remove", "watchpoint_set", "z80_state",
	}
	if len(got) != len(want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tools = %v, want %v", got, want)
		}
	}

	status, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "blastem_status", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if status.IsError {
		t.Fatalf("blastem_status returned tool error: %+v", status.Content)
	}

	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{"blastem_stop", map[string]any{}},
		{"button_down", map[string]any{"pad": 1, "button": "a"}},
		{"screenshot", map[string]any{}},
		{"paused_screenshot", map[string]any{}},
		{"frame_trace", map[string]any{"frames": 1, "watches": []map[string]any{{"address": 0xFF0000}}}},
		{"vdp_layout", map[string]any{}},
		{"line_trace", map[string]any{"frames": 1}},
		{"z80_state", map[string]any{}},
		{"state_save", map[string]any{"name": "x"}},
		{"state_list", map[string]any{}},
		{"reset", map[string]any{}},
		{"button_press", map[string]any{"pad": 1, "button": "a", "frames": 1}},
		{"input_sequence", map[string]any{"frames": 1, "events": []any{}}},
		{"vdp_snapshot", map[string]any{}},
		{"vram_tiles", map[string]any{}},
		{"cpu_registers", map[string]any{}},
		{"memory_read", map[string]any{"address": 0, "size": 1}},
		{"breakpoint_set", map[string]any{"address": 0x200}},
	} {
		result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: test.name, Arguments: test.args})
		if err != nil {
			t.Fatalf("%s protocol error: %v", test.name, err)
		}
		if !result.IsError {
			t.Fatalf("%s succeeded without a running session", test.name)
		}
	}
}
