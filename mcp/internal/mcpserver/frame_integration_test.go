package mcpserver

import (
	"bytes"
	"context"
	encodingbinary "encoding/binary"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/akiyan/blastem/mcp/internal/control"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFrameMCPContract(t *testing.T) {
	binary, rom := os.Getenv("BLASTEM_INTEGRATION_BINARY"), os.Getenv("BLASTEM_FIXTURE_ROM")
	if binary == "" || rom == "" {
		t.Skip("requires built core and fixture")
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
	callOK(t, ctx, session, "blastem_start", map[string]any{"binary_path": binary, "rom_path": rom, "save_dir": t.TempDir()})
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
	ram := callOK(t, ctx, session, "ram_read", map[string]any{"address": 0xFF0011, "size": 3})
	rawRAM, _ := json.Marshal(ram.StructuredContent)
	var observed control.RAMResult
	if err := json.Unmarshal(rawRAM, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Hex != "223344" || observed.Frame != uint64(advanced.Frame) {
		t.Fatalf("wrong RAM observation: %+v", observed)
	}
	before := callOK(t, ctx, session, "paused_screenshot", map[string]any{}).Content[0].(*mcp.ImageContent).Data
	callOK(t, ctx, session, "button_down", map[string]any{"pad": 1, "button": "a"})
	callOK(t, ctx, session, "frame_advance", map[string]any{"frames": 3})
	after := callOK(t, ctx, session, "paused_screenshot", map[string]any{}).Content[0].(*mcp.ImageContent).Data
	if bytes.Equal(before, after) {
		t.Fatal("input failed to change cached image")
	}
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
