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

type traceWatchInput struct {
	Name    string `json:"name,omitempty" jsonschema:"label used for this value in the result; defaults to its index"`
	Space   string `json:"space,omitempty" jsonschema:"ram (default; 68000 address FF0000..FFFFFF), cram (byte offset 0..127), vsram (byte offset), vdp_reg (register 0..23), vram (byte address), z80_ram (0..1FFF), or z80_frame (size 4; address 0 executed, 1 BUSREQ-stopped, 2 reset Z80 cycles in the last completed frame)"`
	Address uint32 `json:"address,omitempty" jsonschema:"address or offset in the selected space (or use symbol)"`
	Symbol  string `json:"symbol,omitempty" jsonschema:"symbol name, optionally NAME+OFFSET; requires symbols_path; name defaults to it"`
	Size    uint32 `json:"size,omitempty" jsonschema:"big-endian width in bytes: 1, 2, or 4; default 2 for cram/vsram, otherwise 1"`
}

type traceUntilInput struct {
	Watch string  `json:"watch" jsonschema:"watch name or zero-based index"`
	Op    string  `json:"op" jsonschema:"eq, ne, or changed (compared with the previous frame)"`
	Value *uint32 `json:"value,omitempty" jsonschema:"comparison value for eq/ne, after masking"`
	Mask  *uint32 `json:"mask,omitempty" jsonschema:"bit mask applied before comparison; default all bits"`
}

type traceInput struct {
	Frames     uint32            `json:"frames" jsonschema:"maximum frames to run, 1..36000"`
	Watches    []traceWatchInput `json:"watches" jsonschema:"1..16 values sampled at every frame boundary"`
	Until      *traceUntilInput  `json:"until,omitempty" jsonschema:"optional condition that stops the run at the first matching frame"`
	MaxRecords int               `json:"max_records,omitempty" jsonschema:"records returned inline, default 500, at most 5000; the artifact keeps every record"`
}

type traceRecord struct {
	Frame   uint32            `json:"frame"`
	Values  map[string]uint32 `json:"values"`
	Changed []string          `json:"changed,omitempty"`
}

type traceOutput struct {
	Trace     control.TraceResult `json:"trace"`
	Watches   []string            `json:"watches"`
	Records   []traceRecord       `json:"records"`
	Truncated bool                `json:"truncated"`
	Artifact  artifact.Info       `json:"artifact"`
}

func (a *App) frameTrace(ctx context.Context, _ *mcp.CallToolRequest, in traceInput) (*mcp.CallToolResult, traceOutput, error) {
	limit := in.MaxRecords
	if limit == 0 {
		limit = 500
	}
	if limit < 1 || limit > 5000 {
		return nil, traceOutput{}, fmt.Errorf("max_records must be in 1..5000")
	}
	names := make([]string, len(in.Watches))
	watches := make([]control.TraceWatch, len(in.Watches))
	index := map[string]uint32{}
	for i, w := range in.Watches {
		space := w.Space
		if space == "" {
			space = "ram"
		}
		size := w.Size
		if size == 0 {
			size = 1
			if space == "cram" || space == "vsram" {
				size = 2
			}
		}
		address, err := a.address(w.Symbol, w.Address)
		if err != nil {
			return nil, traceOutput{}, err
		}
		name := w.Name
		if name == "" && w.Symbol != "" {
			name = w.Symbol
		}
		if name == "" {
			name = strconv.Itoa(i)
		}
		if _, duplicate := index[name]; duplicate {
			return nil, traceOutput{}, fmt.Errorf("duplicate watch name %q", name)
		}
		index[name] = uint32(i)
		names[i] = name
		watches[i] = control.TraceWatch{Space: space, Address: address, Size: size}
	}
	var until *control.TraceUntil
	if in.Until != nil {
		position, ok := index[in.Until.Watch]
		if !ok {
			return nil, traceOutput{}, fmt.Errorf("until.watch %q does not name a watch", in.Until.Watch)
		}
		until = &control.TraceUntil{Watch: position, Op: in.Until.Op, Value: in.Until.Value, Mask: in.Until.Mask}
	}
	client, dir, err := a.session.Control()
	if err != nil {
		return nil, traceOutput{}, err
	}
	path := filepath.Join(dir, fmt.Sprintf("trace-%06d.jsonl", a.serial.Add(1)))
	result, err := client.TraceContext(ctx, path, in.Frames, watches, until)
	if err != nil {
		return nil, traceOutput{}, err
	}
	info, err := artifact.Inspect(path)
	if err != nil {
		return nil, traceOutput{}, err
	}
	records, truncated, err := readTrace(path, names, limit)
	if err != nil {
		return nil, traceOutput{}, err
	}
	return nil, traceOutput{Trace: result, Watches: names, Records: records, Truncated: truncated, Artifact: info}, nil
}

func readTrace(path string, names []string, limit int) ([]traceRecord, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	var records []traceRecord
	var previous []uint32
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var raw struct {
			Frame  uint32   `json:"frame"`
			Values []uint32 `json:"values"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
			return nil, false, fmt.Errorf("invalid trace record: %w", err)
		}
		if len(raw.Values) != len(names) {
			return nil, false, fmt.Errorf("trace record has %d values, expected %d", len(raw.Values), len(names))
		}
		if len(records) == limit {
			return records, true, nil
		}
		record := traceRecord{Frame: raw.Frame, Values: make(map[string]uint32, len(names))}
		for i, v := range raw.Values {
			record.Values[names[i]] = v
			if previous != nil && previous[i] != v {
				record.Changed = append(record.Changed, names[i])
			}
		}
		previous = raw.Values
		records = append(records, record)
	}
	return records, false, scanner.Err()
}
