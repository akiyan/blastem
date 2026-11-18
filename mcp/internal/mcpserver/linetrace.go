package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/akiyan/blastem/mcp/internal/artifact"
	"github.com/akiyan/blastem/mcp/internal/control"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type lineRange struct {
	From int `json:"from" jsonschema:"first VDP line (vcounter) to include"`
	To   int `json:"to" jsonschema:"last VDP line to include"`
}

type lineTraceInput struct {
	Frames    uint32     `json:"frames" jsonschema:"frames to run, 1..600"`
	Kinds     []string   `json:"kinds,omitempty" jsonschema:"event kinds: cram, vsram, reg, int (interrupt acknowledge and its RTE); default all"`
	Lines     *lineRange `json:"lines,omitempty" jsonschema:"only return events on these lines; the summary still covers all events"`
	MaxEvents int        `json:"max_events,omitempty" jsonschema:"events returned inline, default 2000, at most 20000; the artifact keeps every event"`
}

type lineEvent struct {
	Frame   uint32 `json:"frame"`
	Line    uint32 `json:"line" jsonschema:"VDP vcounter when the event took effect"`
	Slot    uint32 `json:"slot" jsonschema:"horizontal slot (hcounter/2) within the line"`
	Kind    string `json:"kind" jsonschema:"cram, vsram, reg, int, or rte"`
	Address *int   `json:"address,omitempty" jsonschema:"CRAM/VSRAM byte offset"`
	Reg     *int   `json:"reg,omitempty"`
	Value   *int   `json:"value,omitempty"`
	Level   *int   `json:"level,omitempty" jsonschema:"interrupt level (4 horizontal, 6 vertical); null for an RTE without a recorded interrupt"`
	Cycles  *int   `json:"cycles,omitempty" jsonschema:"68000 cycles from interrupt acknowledge to RTE"`
}

type interruptSummary struct {
	Count     int     `json:"count"`
	FirstLine uint32  `json:"first_line"`
	LastLine  uint32  `json:"last_line"`
	MinCycles int     `json:"min_cycles"`
	MaxCycles int     `json:"max_cycles"`
	AvgCycles float64 `json:"avg_cycles"`
}

type lineTraceSummary struct {
	ByKind     map[string]int              `json:"by_kind"`
	Interrupts map[string]interruptSummary `json:"interrupts" jsonschema:"by level; cycles come from the matching RTE"`
	WriteLines int                         `json:"write_lines" jsonschema:"distinct frame/line pairs with CRAM, VSRAM, or register writes"`
}

type lineTraceOutput struct {
	Trace     control.LineTraceResult `json:"trace"`
	Summary   lineTraceSummary        `json:"summary"`
	Events    []lineEvent             `json:"events"`
	Truncated bool                    `json:"truncated"`
	Artifact  artifact.Info           `json:"artifact"`
}

func (a *App) lineTrace(ctx context.Context, _ *mcp.CallToolRequest, in lineTraceInput) (*mcp.CallToolResult, lineTraceOutput, error) {
	limit := in.MaxEvents
	if limit == 0 {
		limit = 2000
	}
	if limit < 1 || limit > 20000 {
		return nil, lineTraceOutput{}, fmt.Errorf("max_events must be in 1..20000")
	}
	client, dir, err := a.session.Control()
	if err != nil {
		return nil, lineTraceOutput{}, err
	}
	path := filepath.Join(dir, fmt.Sprintf("lines-%06d.jsonl", a.serial.Add(1)))
	result, err := client.LineTraceContext(ctx, path, in.Frames, in.Kinds)
	if err != nil {
		return nil, lineTraceOutput{}, err
	}
	info, err := artifact.Inspect(path)
	if err != nil {
		return nil, lineTraceOutput{}, err
	}
	out := lineTraceOutput{Trace: result, Artifact: info, Events: []lineEvent{},
		Summary: lineTraceSummary{ByKind: map[string]int{}, Interrupts: map[string]interruptSummary{}}}
	file, err := os.Open(path)
	if err != nil {
		return nil, lineTraceOutput{}, err
	}
	defer file.Close()
	writes := map[[2]uint32]bool{}
	totals := map[string]int{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var e lineEvent
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return nil, lineTraceOutput{}, fmt.Errorf("invalid line trace event: %w", err)
		}
		out.Summary.ByKind[e.Kind]++
		switch e.Kind {
		case "cram", "vsram", "reg":
			writes[[2]uint32{e.Frame, e.Line}] = true
		case "int":
			key := strconv.Itoa(*e.Level)
			s, seen := out.Summary.Interrupts[key]
			if !seen {
				s.FirstLine, s.MinCycles = e.Line, -1
			}
			s.Count++
			s.LastLine = e.Line
			out.Summary.Interrupts[key] = s
		case "rte":
			if e.Level != nil && e.Cycles != nil {
				key := strconv.Itoa(*e.Level)
				s := out.Summary.Interrupts[key]
				if s.MinCycles < 0 || *e.Cycles < s.MinCycles {
					s.MinCycles = *e.Cycles
				}
				if *e.Cycles > s.MaxCycles {
					s.MaxCycles = *e.Cycles
				}
				totals[key] += *e.Cycles
				out.Summary.Interrupts[key] = s
			}
		}
		if in.Lines != nil && (int(e.Line) < in.Lines.From || int(e.Line) > in.Lines.To) {
			continue
		}
		if len(out.Events) == limit {
			out.Truncated = true
			continue
		}
		out.Events = append(out.Events, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, lineTraceOutput{}, err
	}
	for key, s := range out.Summary.Interrupts {
		if s.MinCycles < 0 {
			s.MinCycles = 0
		}
		if s.Count > 0 {
			s.AvgCycles = float64(totals[key]) / float64(s.Count)
		}
		out.Summary.Interrupts[key] = s
	}
	out.Summary.WriteLines = len(writes)
	return nil, out, nil
}
