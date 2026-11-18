package vdp

import (
	"fmt"
	"hash/crc32"
	"strings"
)

// Layout is a text-oriented decoding of the Mode 5 display state: what each
// visible screen cell of each plane points at, and which sprites are linked.
// It is derived only from VRAM/VSRAM/registers; no image is rendered.

type Mode struct {
	Width          int  `json:"width" jsonschema:"active display width in pixels"`
	Height         int  `json:"height" jsonschema:"active display height in pixels"`
	H40            bool `json:"h40"`
	V30            bool `json:"v30"`
	Interlace      int  `json:"interlace" jsonschema:"0 none, 1 normal, 3 double resolution"`
	DisplayEnabled bool `json:"display_enabled"`
	Shadow         bool `json:"shadow_highlight"`
}

type ScrollRun struct {
	From  int `json:"from" jsonschema:"first screen line (horizontal scroll) or 16-pixel column (vertical scroll)"`
	To    int `json:"to" jsonschema:"last line or column, inclusive"`
	Value int `json:"value" jsonschema:"scroll value in pixels (10-bit signed for horizontal)"`
}

type PlaneScroll struct {
	Horizontal []ScrollRun `json:"horizontal"`
	Vertical   []ScrollRun `json:"vertical"`
}

type Scroll struct {
	HorizontalMode string      `json:"horizontal_mode" jsonschema:"full, cell (8 lines), or line"`
	VerticalMode   string      `json:"vertical_mode" jsonschema:"full or column (16 pixels)"`
	PlaneA         PlaneScroll `json:"plane_a"`
	PlaneB         PlaneScroll `json:"plane_b"`
}

type Cell struct {
	X        int    `json:"x" jsonschema:"screen cell column"`
	Y        int    `json:"y" jsonschema:"screen cell row"`
	Tile     int    `json:"tile"`
	Palette  int    `json:"palette"`
	Priority bool   `json:"priority"`
	HFlip    bool   `json:"hflip"`
	VFlip    bool   `json:"vflip"`
	Pattern  string `json:"pattern,omitempty" jsonschema:"CRC32 of the 32-byte tile pattern"`
}

type Plane struct {
	Name     string   `json:"name" jsonschema:"a, b, or window"`
	Base     int      `json:"base" jsonschema:"name table VRAM address"`
	Width    int      `json:"width" jsonschema:"name table width in cells"`
	Height   int      `json:"height" jsonschema:"name table height in cells"`
	NonEmpty int      `json:"non_empty" jsonschema:"cells in the requested rectangle whose entry is not zero"`
	Palettes [4]int   `json:"palettes" jsonschema:"non-empty cell count per palette"`
	Grid     []string `json:"grid,omitempty" jsonschema:"one string per screen row: tokens TILE/PALETTE with * priority, h hflip, v vflip; . for an empty entry; - outside the window area"`
	Cells    []Cell   `json:"cells,omitempty"`
}

type Sprite struct {
	Index    int    `json:"index" jsonschema:"sprite table entry"`
	Order    int    `json:"order" jsonschema:"position in the link chain (draw priority, 0 first)"`
	X        int    `json:"x" jsonschema:"screen x (table value minus 128)"`
	Y        int    `json:"y" jsonschema:"screen y (table value minus 128)"`
	Width    int    `json:"width" jsonschema:"cells"`
	Height   int    `json:"height" jsonschema:"cells"`
	Tile     int    `json:"tile"`
	Palette  int    `json:"palette"`
	Priority bool   `json:"priority"`
	HFlip    bool   `json:"hflip"`
	VFlip    bool   `json:"vflip"`
	Visible  bool   `json:"visible" jsonschema:"overlaps the active display"`
	Pattern  string `json:"pattern,omitempty" jsonschema:"CRC32 of all tile patterns used by the sprite"`
}

type Sprites struct {
	Base     int      `json:"base"`
	Linked   int      `json:"linked" jsonschema:"entries reached through the link chain"`
	Visible  int      `json:"visible"`
	Limit    int      `json:"limit" jsonschema:"hardware table limit (80 in H40, 64 in H32)"`
	Overflow []int    `json:"overflow_lines,omitempty" jsonschema:"screen lines exceeding the per-line sprite count limit"`
	Entries  []Sprite `json:"entries"`
	Problems []string `json:"problems,omitempty"`
}

type Rect struct {
	X      int `json:"x" jsonschema:"first screen cell column"`
	Y      int `json:"y" jsonschema:"first screen cell row"`
	Width  int `json:"width" jsonschema:"columns; 0 means to the right edge"`
	Height int `json:"height" jsonschema:"rows; 0 means to the bottom edge"`
}

type LayoutOptions struct {
	Planes        []string
	Sprites       bool
	Rect          *Rect
	Cells         bool
	Patterns      bool
	HiddenSprites bool
}

type Layout struct {
	Mode     Mode     `json:"mode"`
	Backdrop [2]int   `json:"backdrop" jsonschema:"palette and colour index"`
	Scroll   Scroll   `json:"scroll"`
	Rect     Rect     `json:"rect"`
	Planes   []Plane  `json:"planes"`
	Sprites  *Sprites `json:"sprites,omitempty"`
}

func (s Snapshot) reg(i int) int {
	if i < len(s.Regs) {
		return int(s.Regs[i])
	}
	return 0
}

func (s Snapshot) word(address int) int {
	address &= 0xFFFF
	return int(s.VRAM[address])<<8 | int(s.VRAM[(address+1)&0xFFFF])
}

func (s Snapshot) mode() Mode {
	m := Mode{H40: s.reg(12)&0x81 != 0, V30: s.reg(1)&0x08 != 0, Interlace: (s.reg(12) >> 1) & 3,
		DisplayEnabled: s.reg(1)&0x40 != 0, Shadow: s.reg(12)&0x08 != 0}
	m.Width, m.Height = 256, 224
	if m.H40 {
		m.Width = 320
	}
	if m.V30 {
		m.Height = 240
	}
	return m
}

func planeCells(code int) int {
	switch code & 3 {
	case 1:
		return 64
	case 3:
		return 128
	}
	return 32
}

func (s Snapshot) patternHash(tile, count int) string {
	var data []byte
	for i := 0; i < count; i++ {
		start := ((tile + i) & 0x7FF) * TileSize
		data = append(data, s.VRAM[start%len(s.VRAM):start%len(s.VRAM)+TileSize]...)
	}
	return fmt.Sprintf("%08x", crc32.ChecksumIEEE(data))
}

func signed10(v int) int {
	v &= 0x3FF
	if v&0x200 != 0 {
		v -= 0x400
	}
	return v
}

// hscroll returns the horizontal scroll of plane (0 A, 1 B) for a screen line.
func (s Snapshot) hscroll(plane, line int) int {
	base := (s.reg(13) & 0x3F) << 10
	switch s.reg(11) & 3 {
	case 2:
		line &^= 7
	case 3:
	default:
		line = 0
	}
	return signed10(s.word(base + line*4 + plane*2))
}

// vscroll returns the vertical scroll of plane for a screen pixel column.
func (s Snapshot) vscroll(plane, x int) int {
	index := plane
	if s.reg(11)&4 != 0 && x >= 0 {
		index = (x/16)*2 + plane
	}
	if index >= len(s.VSRAM) {
		return 0
	}
	return int(s.VSRAM[index] & 0x7FF)
}

func runs(count int, value func(int) int) []ScrollRun {
	var out []ScrollRun
	for i := 0; i < count; i++ {
		v := value(i)
		if len(out) > 0 && out[len(out)-1].Value == v {
			out[len(out)-1].To = i
			continue
		}
		out = append(out, ScrollRun{From: i, To: i, Value: v})
	}
	return out
}

func token(entry int) string {
	if entry == 0 {
		return "."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%x/%d", entry&0x7FF, (entry>>13)&3)
	if entry&0x8000 != 0 {
		b.WriteByte('*')
	}
	if entry&0x0800 != 0 {
		b.WriteByte('h')
	}
	if entry&0x1000 != 0 {
		b.WriteByte('v')
	}
	return b.String()
}

func (s Snapshot) Layout(opts LayoutOptions) (Layout, error) {
	if len(s.VRAM) != 0x10000 || len(s.Regs) < 19 {
		return Layout{}, fmt.Errorf("layout requires a full 64 KiB VRAM and 19+ registers")
	}
	if s.reg(1)&0x04 == 0 {
		return Layout{}, fmt.Errorf("VDP is not in Mode 5")
	}
	m := s.mode()
	l := Layout{Mode: m, Backdrop: [2]int{(s.reg(7) >> 4) & 3, s.reg(7) & 15}}
	columns, rows := m.Width/8, m.Height/8
	r := Rect{Width: columns, Height: rows}
	if opts.Rect != nil {
		r = *opts.Rect
		if r.Width == 0 {
			r.Width = columns - r.X
		}
		if r.Height == 0 {
			r.Height = rows - r.Y
		}
		if r.X < 0 || r.Y < 0 || r.Width < 1 || r.Height < 1 || r.X+r.Width > columns || r.Y+r.Height > rows {
			return Layout{}, fmt.Errorf("rect must lie within %dx%d screen cells", columns, rows)
		}
	}
	l.Rect = r
	l.Scroll.HorizontalMode = map[int]string{0: "full", 1: "full", 2: "cell", 3: "line"}[s.reg(11)&3]
	l.Scroll.VerticalMode = "full"
	if s.reg(11)&4 != 0 {
		l.Scroll.VerticalMode = "column"
	}
	for plane, target := range []*PlaneScroll{&l.Scroll.PlaneA, &l.Scroll.PlaneB} {
		target.Horizontal = runs(m.Height, func(line int) int { return s.hscroll(plane, line) })
		target.Vertical = runs((m.Width+15)/16, func(column int) int { return s.vscroll(plane, column*16) })
	}
	width, height := planeCells(s.reg(16)), planeCells(s.reg(16)>>4)
	windowBase := (s.reg(3) & 0x3E) << 10
	windowWidth := 32
	if m.H40 {
		windowBase &^= 0x0800
		windowWidth = 64
	}
	inWindow := func(cx, cy int) bool {
		h, v := (s.reg(17)&0x1F)*2, s.reg(18)&0x1F
		horizontal := (s.reg(17)&0x80 == 0 && cx < h) || (s.reg(17)&0x80 != 0 && cx >= h)
		vertical := (s.reg(18)&0x80 == 0 && cy < v) || (s.reg(18)&0x80 != 0 && cy >= v)
		return horizontal || vertical
	}
	planes := opts.Planes
	if len(planes) == 0 {
		planes = []string{"a", "b", "window"}
	}
	for _, name := range planes {
		p := Plane{Name: name, Width: width, Height: height}
		var lookup func(cx, cy int) (int, bool)
		switch name {
		case "a", "b":
			index := 0
			p.Base = (s.reg(2) & 0x38) << 10
			if name == "b" {
				index = 1
				p.Base = (s.reg(4) & 7) << 13
			}
			lookup = func(cx, cy int) (int, bool) {
				if index == 0 && inWindow(cx, cy) {
					return 0, false
				}
				x := (cx*8 - s.hscroll(index, cy*8)) & (width*8 - 1)
				y := (cy*8 + s.vscroll(index, cx*8)) & (height*8 - 1)
				return s.word(p.Base + 2*((y/8)*width+x/8)), true
			}
		case "window":
			p.Base, p.Width, p.Height = windowBase, windowWidth, 32
			lookup = func(cx, cy int) (int, bool) {
				if !inWindow(cx, cy) {
					return 0, false
				}
				return s.word(p.Base + 2*(cy*windowWidth+cx)), true
			}
		default:
			return Layout{}, fmt.Errorf("unknown plane %q (use a, b, or window)", name)
		}
		for cy := r.Y; cy < r.Y+r.Height; cy++ {
			var row []string
			for cx := r.X; cx < r.X+r.Width; cx++ {
				entry, shown := lookup(cx, cy)
				if !shown {
					row = append(row, "-")
					continue
				}
				row = append(row, token(entry))
				if entry == 0 {
					continue
				}
				p.NonEmpty++
				p.Palettes[(entry>>13)&3]++
				if opts.Cells {
					cell := Cell{X: cx, Y: cy, Tile: entry & 0x7FF, Palette: (entry >> 13) & 3, Priority: entry&0x8000 != 0,
						HFlip: entry&0x0800 != 0, VFlip: entry&0x1000 != 0}
					if opts.Patterns {
						cell.Pattern = s.patternHash(cell.Tile, 1)
					}
					p.Cells = append(p.Cells, cell)
				}
			}
			p.Grid = append(p.Grid, strings.Join(row, " "))
		}
		l.Planes = append(l.Planes, p)
	}
	if opts.Sprites {
		sprites := s.sprites(m, opts)
		l.Sprites = &sprites
	}
	return l, nil
}

func (s Snapshot) sprites(m Mode, opts LayoutOptions) Sprites {
	out := Sprites{Base: (s.reg(5) & 0x7F) << 9, Limit: 64, Entries: []Sprite{}}
	perLine := 16
	if m.H40 {
		out.Base &^= 0x200
		out.Limit = 80
		perLine = 20
	}
	lineCounts := make([]int, m.Height)
	seen := map[int]bool{}
	index := 0
	for order := 0; order < out.Limit; order++ {
		if seen[index] {
			out.Problems = append(out.Problems, fmt.Sprintf("link loop at entry %d", index))
			break
		}
		seen[index] = true
		address := out.Base + index*8
		size := s.word(address+2) >> 8
		attr := s.word(address + 4)
		sp := Sprite{Index: index, Order: order, Y: (s.word(address) & 0x3FF) - 128, X: (s.word(address+6) & 0x1FF) - 128,
			Width: (size>>2)&3 + 1, Height: size&3 + 1, Tile: attr & 0x7FF, Palette: (attr >> 13) & 3,
			Priority: attr&0x8000 != 0, HFlip: attr&0x0800 != 0, VFlip: attr&0x1000 != 0}
		sp.Visible = sp.X < m.Width && sp.X+sp.Width*8 > 0 && sp.Y < m.Height && sp.Y+sp.Height*8 > 0
		for line := max(sp.Y, 0); line < min(sp.Y+sp.Height*8, m.Height); line++ {
			lineCounts[line]++
		}
		if sp.Visible {
			out.Visible++
		}
		if opts.Patterns {
			sp.Pattern = s.patternHash(sp.Tile, sp.Width*sp.Height)
		}
		if sp.Visible || opts.HiddenSprites {
			out.Entries = append(out.Entries, sp)
		}
		out.Linked++
		index = s.word(address+2) & 0x7F
		if index == 0 {
			break
		}
		if index >= out.Limit {
			out.Problems = append(out.Problems, fmt.Sprintf("link %d exceeds table limit %d", index, out.Limit))
			break
		}
	}
	for line, count := range lineCounts {
		if count > perLine {
			out.Overflow = append(out.Overflow, line)
		}
	}
	return out
}
