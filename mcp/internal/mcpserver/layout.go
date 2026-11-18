package mcpserver

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/akiyan/blastem/mcp/internal/artifact"
	"github.com/akiyan/blastem/mcp/internal/vdp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type layoutInput struct {
	Planes        []string  `json:"planes,omitempty" jsonschema:"planes to decode: a, b, window; default all"`
	Sprites       *bool     `json:"sprites,omitempty" jsonschema:"decode the sprite link chain; default true"`
	HiddenSprites bool      `json:"hidden_sprites,omitempty" jsonschema:"also list linked sprites outside the display"`
	Rect          *vdp.Rect `json:"rect,omitempty" jsonschema:"screen cell rectangle; default the whole display"`
	Cells         bool      `json:"cells,omitempty" jsonschema:"also return one object per non-empty cell"`
	Patterns      bool      `json:"patterns,omitempty" jsonschema:"add CRC32 of tile pattern data to cells and sprites"`
}

type layoutOutput struct {
	Frame    uint64        `json:"frame"`
	Layout   vdp.Layout    `json:"layout"`
	Artifact artifact.Info `json:"artifact" jsonschema:"KITVDMP1 snapshot the layout was decoded from"`
}

func (a *App) vdpLayout(_ context.Context, _ *mcp.CallToolRequest, in layoutInput) (*mcp.CallToolResult, layoutOutput, error) {
	client, dir, err := a.session.Control()
	if err != nil {
		return nil, layoutOutput{}, err
	}
	path := filepath.Join(dir, fmt.Sprintf("layout-%06d.kitvdmp", a.serial.Add(1)))
	dump, err := client.VDPDump(path)
	if err != nil {
		return nil, layoutOutput{}, err
	}
	snapshot, err := vdp.Read(path)
	if err != nil {
		return nil, layoutOutput{}, err
	}
	sprites := in.Sprites == nil || *in.Sprites
	layout, err := snapshot.Layout(vdp.LayoutOptions{Planes: in.Planes, Sprites: sprites, Rect: in.Rect, Cells: in.Cells,
		Patterns: in.Patterns, HiddenSprites: in.HiddenSprites})
	if err != nil {
		return nil, layoutOutput{}, err
	}
	info, err := artifact.Inspect(path)
	if err != nil {
		return nil, layoutOutput{}, err
	}
	return nil, layoutOutput{Frame: dump.Frame, Layout: layout, Artifact: info}, nil
}
