package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/akiyan/blastem/mcp/internal/control"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type stateInput struct {
	Name string `json:"name" jsonschema:"state name: 1..64 of A-Z a-z 0-9 . _ -, starting with a letter or digit"`
}

type stateInfo struct {
	Name     string    `json:"name"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified"`
}

type stateListOutput struct {
	Directory string      `json:"directory"`
	States    []stateInfo `json:"states"`
}

func (a *App) stateSave(_ context.Context, _ *mcp.CallToolRequest, in stateInput) (*mcp.CallToolResult, control.StateResult, error) {
	client, _, err := a.session.Control()
	if err != nil {
		return nil, control.StateResult{}, err
	}
	result, err := client.SaveState(in.Name)
	return nil, result, err
}

func (a *App) stateLoad(_ context.Context, _ *mcp.CallToolRequest, in stateInput) (*mcp.CallToolResult, control.StateResult, error) {
	client, _, err := a.session.Control()
	if err != nil {
		return nil, control.StateResult{}, err
	}
	result, err := client.LoadState(in.Name)
	return nil, result, err
}

func (a *App) stateList(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, stateListOutput, error) {
	dir := a.session.Status().StateDir
	if dir == "" {
		return nil, stateListOutput{}, fmt.Errorf("no BlastEM session is running")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, stateListOutput{}, err
	}
	out := stateListOutput{Directory: dir, States: []stateInfo{}}
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".state")
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out.States = append(out.States, stateInfo{Name: name, Bytes: info.Size(), Modified: info.ModTime()})
	}
	sort.Slice(out.States, func(i, j int) bool { return out.States[i].Name < out.States[j].Name })
	return nil, out, nil
}

func (a *App) reset(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, control.ResetResult, error) {
	client, _, err := a.session.Control()
	if err != nil {
		return nil, control.ResetResult{}, err
	}
	if err := client.ReleaseAll(); err != nil {
		return nil, control.ResetResult{}, err
	}
	result, err := client.SoftReset()
	return nil, result, err
}

type pressInput struct {
	Pad    int    `json:"pad" jsonschema:"controller port number, 1 or 2"`
	Button string `json:"button" jsonschema:"up, down, left, right, a, b, c, x, y, z, start, or mode"`
	Frames uint32 `json:"frames" jsonschema:"frames to hold the button, 1..600; released at the final frame boundary"`
}

func (a *App) buttonPress(ctx context.Context, _ *mcp.CallToolRequest, in pressInput) (*mcp.CallToolResult, control.FrameResult, error) {
	client, _, err := a.session.Control()
	if err != nil {
		return nil, control.FrameResult{}, err
	}
	if err := client.ButtonDown(in.Pad, in.Button); err != nil {
		return nil, control.FrameResult{}, err
	}
	result, runErr := client.AdvanceContext(ctx, in.Frames)
	releaseErr := client.ButtonUp(in.Pad, in.Button)
	return nil, result, errors.Join(runErr, releaseErr)
}

type inputEvent struct {
	Frame  uint32 `json:"frame" jsonschema:"frames after the start of the sequence (0 = before the first frame)"`
	Pad    int    `json:"pad" jsonschema:"controller port number, 1 or 2"`
	Button string `json:"button"`
	Action string `json:"action" jsonschema:"down or up"`
}

type sequenceInput struct {
	Events   []inputEvent `json:"events" jsonschema:"input changes; applied in frame order, then listed order"`
	Frames   uint32       `json:"frames" jsonschema:"total frames to run, at least the last event frame; at most 36000"`
	KeepHeld bool         `json:"keep_held,omitempty" jsonschema:"leave buttons pressed at the end instead of releasing them"`
}

type sequenceOutput struct {
	Applied int                 `json:"applied"`
	Result  control.FrameResult `json:"result"`
}

func (a *App) inputSequence(ctx context.Context, _ *mcp.CallToolRequest, in sequenceInput) (*mcp.CallToolResult, sequenceOutput, error) {
	if in.Frames < 1 || in.Frames > 36000 {
		return nil, sequenceOutput{}, fmt.Errorf("frames must be in 1..36000")
	}
	events := append([]inputEvent(nil), in.Events...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Frame < events[j].Frame })
	for _, e := range events {
		if e.Frame > in.Frames || (e.Action != "down" && e.Action != "up") {
			return nil, sequenceOutput{}, fmt.Errorf("invalid event %+v: frame must be <= frames and action down or up", e)
		}
	}
	client, _, err := a.session.Control()
	if err != nil {
		return nil, sequenceOutput{}, err
	}
	out := sequenceOutput{}
	run := func(frames uint32) error {
		for frames > 0 {
			step := min(frames, 600)
			result, err := client.AdvanceContext(ctx, step)
			if err != nil {
				return err
			}
			out.Result = result
			if result.Interrupted {
				return fmt.Errorf("sequence interrupted at frame %d", result.Frame)
			}
			frames -= step
		}
		return nil
	}
	position := uint32(0)
	var failure error
	for _, e := range events {
		if failure = run(e.Frame - position); failure != nil {
			break
		}
		position = e.Frame
		if e.Action == "down" {
			failure = client.ButtonDown(e.Pad, e.Button)
		} else {
			failure = client.ButtonUp(e.Pad, e.Button)
		}
		if failure != nil {
			break
		}
		out.Applied++
	}
	if failure == nil {
		failure = run(in.Frames - position)
	}
	if failure != nil || !in.KeepHeld {
		failure = errors.Join(failure, client.ReleaseAll())
	}
	return nil, out, failure
}
