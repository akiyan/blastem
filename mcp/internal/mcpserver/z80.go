package mcpserver

import (
	"context"

	"github.com/akiyan/blastem/mcp/internal/control"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type z80Input struct {
	Address *uint32 `json:"address,omitempty" jsonschema:"optional Z80 RAM address 0..1FFF to read"`
	Size    uint32  `json:"size,omitempty" jsonschema:"bytes to read with address, 1..256; default 16"`
}

type z80Output struct {
	State control.Z80State   `json:"state"`
	RAM   *control.RAMResult `json:"ram,omitempty"`
}

func (a *App) z80State(_ context.Context, _ *mcp.CallToolRequest, in z80Input) (*mcp.CallToolResult, z80Output, error) {
	client, _, err := a.session.Control()
	if err != nil {
		return nil, z80Output{}, err
	}
	state, err := client.Z80State()
	if err != nil {
		return nil, z80Output{}, err
	}
	out := z80Output{State: state}
	if in.Address != nil {
		size := in.Size
		if size == 0 {
			size = 16
		}
		ram, err := client.Z80Read(*in.Address, size)
		if err != nil {
			return nil, z80Output{}, err
		}
		out.RAM = &ram
	}
	return nil, out, nil
}
