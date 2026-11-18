package mcpserver

import (
	"bytes"
	"context"
	encodingbinary "encoding/binary"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akiyan/blastem/mcp/internal/control"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFrameMCPContract(t *testing.T) {
	binary, rom := os.Getenv("BLASTEM_INTEGRATION_BINARY"), os.Getenv("BLASTEM_FIXTURE_ROM")
	if binary == "" || rom == "" || os.Getenv("BLASTEM_FIXTURE_SYMBOLS") == "" {
		t.Skip("requires built core, fixture, and fixture symbols")
	}
	t.Setenv("SDL_AUDIODRIVER", "dummy")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	app := New("frame-test")
	t.Cleanup(func() { _ = app.Close() })
	server, err := app.Server().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "frame-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	callOK(t, ctx, session, "blastem_start", withHeadless(map[string]any{"binary_path": binary, "rom_path": rom, "save_dir": t.TempDir(),
		"symbols_path": os.Getenv("BLASTEM_FIXTURE_SYMBOLS")}))
	frame := func(result *mcp.CallToolResult) control.FrameResult {
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var f control.FrameResult
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	initial := frame(callOK(t, ctx, session, "frame_pause", map[string]any{}))
	callOK(t, ctx, session, "speed_set", map[string]any{"percent": 500})
	advanced := frame(callOK(t, ctx, session, "frame_advance", map[string]any{"frames": 5}))
	if !advanced.Paused || advanced.Advanced != 5 || advanced.Frame != initial.Frame+5 {
		t.Fatalf("wrong frame result: %+v %+v", initial, advanced)
	}
	ram := callOK(t, ctx, session, "ram_read", map[string]any{"symbol": "fixture_scratch+1", "size": 3})
	rawRAM, _ := json.Marshal(ram.StructuredContent)
	var observed control.RAMResult
	if err := json.Unmarshal(rawRAM, &observed); err != nil {
		t.Fatal(err)
	}
	if location := ram.StructuredContent.(map[string]any)["location"]; observed.Hex != "223344" || observed.Frame != uint64(advanced.Frame) || location != "fixture_scratch+0x1" {
		t.Fatalf("wrong RAM observation: %+v", observed)
	}
	before := callOK(t, ctx, session, "paused_screenshot", map[string]any{}).Content[0].(*mcp.ImageContent).Data
	callOK(t, ctx, session, "button_down", map[string]any{"pad": 1, "button": "a"})
	callOK(t, ctx, session, "frame_advance", map[string]any{"frames": 3})
	after := callOK(t, ctx, session, "paused_screenshot", map[string]any{}).Content[0].(*mcp.ImageContent).Data
	if bytes.Equal(before, after) {
		t.Fatal("input failed to change cached image")
	}
	// Text-only observation: A is held, so the backdrop register stays 2 and the counter ticks.
	watches := []map[string]any{{"name": "counter", "symbol": "fixture_frame_counter", "size": 4}, {"name": "backdrop", "space": "vdp_reg", "address": 7}}
	lookup := callOK(t, ctx, session, "symbol_lookup", map[string]any{"name": "fixture_", "address": 0xFF0006}).StructuredContent.(map[string]any)
	if lookup["location"] != "fixture_frame_counter+0x2" || len(lookup["matches"].([]any)) < 5 {
		t.Fatalf("symbol lookup = %+v", lookup)
	}
	traced := traceOutputOf(t, callOK(t, ctx, session, "frame_trace", map[string]any{"frames": 30, "watches": watches}))
	if traced.Trace.Stop != "frames" || traced.Trace.Advanced != 30 || len(traced.Records) != 31 || traced.Truncated {
		t.Fatalf("wrong trace: %+v", traced.Trace)
	}
	for i, record := range traced.Records {
		if record.Values["backdrop"] != 2 || (i > 0 && (record.Frame != traced.Records[i-1].Frame+1 || len(record.Changed) != 1 || record.Changed[0] != "counter")) {
			t.Fatalf("wrong trace record %d: %+v", i, record)
		}
	}
	callOK(t, ctx, session, "button_up", map[string]any{"pad": 1, "button": "a"})
	conditional := traceOutputOf(t, callOK(t, ctx, session, "frame_trace", map[string]any{"frames": 600, "watches": watches, "max_records": 1,
		"until": map[string]any{"watch": "backdrop", "op": "eq", "value": 1}}))
	if conditional.Trace.Stop != "until" || conditional.Trace.Advanced > 10 || !conditional.Truncated || len(conditional.Records) != 1 || conditional.Artifact.Size == 0 {
		t.Fatalf("wrong conditional trace: %+v", conditional)
	}
	// Text-only display state: plane grids cover the 40x28 screen and the backdrop register is decoded.
	var layout layoutOutput
	rawLayout, _ := json.Marshal(callOK(t, ctx, session, "vdp_layout", map[string]any{"patterns": true, "cells": true, "planes": []string{"a"}}).StructuredContent)
	if err := json.Unmarshal(rawLayout, &layout); err != nil {
		t.Fatal(err)
	}
	planeA := layout.Layout.Planes[0]
	if layout.Layout.Mode.Width != 320 || len(planeA.Grid) != 28 || len(strings.Fields(planeA.Grid[0])) != 40 || planeA.NonEmpty == 0 ||
		planeA.Palettes[0]+planeA.Palettes[1]+planeA.Palettes[2]+planeA.Palettes[3] != planeA.NonEmpty || len(planeA.Cells) != planeA.NonEmpty ||
		planeA.Cells[0].Pattern == "" || layout.Layout.Backdrop != [2]int{0, 1} || layout.Layout.Sprites == nil || layout.Frame != uint64(conditional.Trace.Frame) {
		t.Fatalf("wrong layout: %+v", layout)
	}
	// Raster timing as text: holding C makes the fixture write CRAM from a per-line interrupt.
	callOK(t, ctx, session, "button_down", map[string]any{"pad": 1, "button": "c"})
	callOK(t, ctx, session, "frame_advance", map[string]any{"frames": 2})
	var lines lineTraceOutput
	rawLines, _ := json.Marshal(callOK(t, ctx, session, "line_trace", map[string]any{"frames": 1, "lines": map[string]any{"from": 100, "to": 109}}).StructuredContent)
	if err := json.Unmarshal(rawLines, &lines); err != nil {
		t.Fatal(err)
	}
	hint := lines.Summary.Interrupts["4"]
	if lines.Trace.Advanced != 1 || hint.Count < 200 || lines.Summary.ByKind["cram"] != hint.Count || hint.MinCycles <= 0 || hint.MaxCycles >= 488 {
		t.Fatalf("wrong line trace summary: %+v", lines.Summary)
	}
	for _, e := range lines.Events {
		if e.Line < 100 || e.Line > 109 {
			t.Fatalf("line filter leaked %+v", e)
		}
	}
	if len(lines.Events) < 20 || lines.Truncated {
		t.Fatalf("wrong filtered events: %d truncated=%v", len(lines.Events), lines.Truncated)
	}
	callOK(t, ctx, session, "button_up", map[string]any{"pad": 1, "button": "c"})
	// Sound CPU: the fixture's Z80 loop runs for the whole frame and its counter lives at 0x1000.
	var z80 z80Output
	rawZ80, _ := json.Marshal(callOK(t, ctx, session, "z80_state", map[string]any{"address": 0, "size": 6}).StructuredContent)
	if err := json.Unmarshal(rawZ80, &z80); err != nil {
		t.Fatal(err)
	}
	if z80.State.Reset || z80.State.HL != 0x1000 || z80.State.LastFrame.Run < 59000 || z80.State.LastFrame.Run > 60500 || z80.RAM == nil || z80.RAM.Hex != "2100103418fd" {
		t.Fatalf("wrong Z80 state: %+v %+v", z80.State, z80.RAM)
	}
	// Named state + scripted input: replaying from the same state gives identical values.
	callOK(t, ctx, session, "state_save", map[string]any{"name": "checkpoint"})
	replay := func() []traceRecord {
		sequence := map[string]any{"frames": 30, "events": []map[string]any{
			{"frame": 0, "pad": 1, "button": "a", "action": "down"}, {"frame": 12, "pad": 1, "button": "a", "action": "up"}}}
		applied := callOK(t, ctx, session, "input_sequence", sequence).StructuredContent.(map[string]any)
		if applied["applied"].(float64) != 2 {
			t.Fatalf("input_sequence = %+v", applied)
		}
		return traceOutputOf(t, callOK(t, ctx, session, "frame_trace", map[string]any{"frames": 20, "watches": []map[string]any{
			{"symbol": "fixture_frame_counter", "size": 4}, {"space": "z80_ram", "address": 0x1000}, {"symbol": "fixture_input_state", "size": 2}}})).Records
	}
	first := replay()
	loaded := callOK(t, ctx, session, "state_load", map[string]any{"name": "checkpoint"}).StructuredContent.(map[string]any)
	second := replay()
	if !reflect.DeepEqual(first, second) || loaded["saved_frame"] != loaded["frame"] {
		t.Fatalf("state replay differs:\n%+v\n%+v\n%+v", first, second, loaded)
	}
	if states := callOK(t, ctx, session, "state_list", map[string]any{}).StructuredContent.(map[string]any)["states"].([]any); len(states) != 1 {
		t.Fatalf("state_list = %+v", states)
	}
	pressed := frame(callOK(t, ctx, session, "button_press", map[string]any{"pad": 1, "button": "b", "frames": 6}))
	if pressed.Advanced != 6 {
		t.Fatalf("button_press = %+v", pressed)
	}
	callOK(t, ctx, session, "reset", map[string]any{})
	afterReset := traceOutputOf(t, callOK(t, ctx, session, "frame_trace", map[string]any{"frames": 10, "watches": []map[string]any{{"symbol": "fixture_frame_counter", "size": 4}}}))
	if last := afterReset.Records[len(afterReset.Records)-1].Values["fixture_frame_counter"]; last > 10 {
		t.Fatalf("frame counter %d after reset", last)
	}
	callOK(t, ctx, session, "button_down", map[string]any{"pad": 1, "button": "a"})
	callOK(t, ctx, session, "frame_advance", map[string]any{"frames": 3})
	callOK(t, ctx, session, "speed_set", map[string]any{"percent": 100})
	// Record the known fixture tone through the public MCP contract.
	recorded := callOK(t, ctx, session, "audio_capture", map[string]any{"frames": 60})
	wav := recorded.Content[0].(*mcp.AudioContent).Data
	if len(wav) < 44 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Fatal("invalid WAV response")
	}
	var metadata audioCaptureOutput
	raw, _ := json.Marshal(recorded.StructuredContent)
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Frames.Advanced != 60 || !metadata.Frames.Paused || metadata.Audio.Channels != 2 || metadata.Audio.Samples == 0 {
		t.Fatalf("invalid audio metadata: %+v", metadata)
	}
	if uint64(len(wav)-44) != metadata.Audio.Samples*4 {
		t.Fatal("sample count does not match WAV")
	}
	var peak int
	var sum, square float64
	for i := 44; i+1 < len(wav); i += 2 {
		value := int(int16(encodingbinary.LittleEndian.Uint16(wav[i : i+2])))
		sum += float64(value)
		square += float64(value * value)
		if value < 0 {
			value = -value
		}
		if value > peak {
			peak = value
		}
	}
	count := float64((len(wav) - 44) / 2)
	variance := square/count - (sum/count)*(sum/count)
	if peak < 100 || variance < 400 {
		t.Fatalf("fixture tone absent: peak=%d", peak)
	}
	callOK(t, ctx, session, "button_up", map[string]any{"pad": 1, "button": "a"})
	type outcome struct {
		result *mcp.CallToolResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "frame_advance", Arguments: map[string]any{"frames": 600}})
		done <- outcome{r, e}
	}()
	time.Sleep(200 * time.Millisecond)
	callOK(t, ctx, session, "frame_interrupt", map[string]any{})
	run := <-done
	if run.err != nil || run.result.IsError {
		t.Fatalf("run failed: %+v", run)
	}
	stopped := frame(run.result)
	if !stopped.Paused || !stopped.Interrupted || stopped.Advanced >= 600 {
		t.Fatalf("interrupt failed: %+v", stopped)
	}
	// Cancelling the specific request closes its stream and stops the core;
	// it cannot interrupt a future operation using a new connection.
	ctrl, _, err := app.session.Control()
	if err != nil {
		t.Fatal(err)
	}
	limited, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	if _, err := ctrl.AdvanceContext(limited, 600); err == nil {
		t.Fatal("cancelled request succeeded")
	}
	again, err := ctrl.Pause()
	if err != nil || !again.Paused {
		t.Fatalf("cancel did not leave recoverable core: %+v %v", again, err)
	}
	final, err := ctrl.Advance(1)
	if err != nil || final.Advanced != 1 {
		t.Fatalf("advance after cancellation: %+v %v", final, err)
	}
	callOK(t, ctx, session, "blastem_stop", map[string]any{})
}

func traceOutputOf(t *testing.T, result *mcp.CallToolResult) traceOutput {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out traceOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
