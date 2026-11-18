package vdp

import (
	"strings"
	"testing"
)

func syntheticSnapshot() Snapshot {
	s := Snapshot{VRAM: make([]byte, 0x10000), CRAM: make([]uint16, 64), VSRAM: make([]uint16, 64), Regs: make([]byte, 24)}
	s.Regs[1] = 0x44  // display on, Mode 5
	s.Regs[2] = 0x30  // plane A 0xC000
	s.Regs[4] = 0x07  // plane B 0xE000
	s.Regs[5] = 0x78  // sprites 0xF000
	s.Regs[7] = 0x21  // backdrop palette 2 colour 1
	s.Regs[12] = 0x81 // H40
	s.Regs[13] = 0x3F // hscroll 0xFC00
	s.Regs[16] = 0x01 // 64x32
	return s
}

func put(s Snapshot, address, value int) {
	s.VRAM[address] = byte(value >> 8)
	s.VRAM[address+1] = byte(value)
}

func TestLayoutScrollsPlanesAndDecodesFlags(t *testing.T) {
	s := syntheticSnapshot()
	put(s, 0xC000, 0x8000|0x4000|0x0800|5) // priority, palette 2, hflip, tile 5
	put(s, 0xFC00, 8)                      // plane A scrolled right one cell
	put(s, 0xE000+2*64, 0x2000|7)          // plane B row 1: palette 1, tile 7
	s.VSRAM[1] = 8                         // plane B scrolled up one cell
	for i := 0; i < TileSize; i++ {
		s.VRAM[5*TileSize+i] = byte(i)
	}
	l, err := s.Layout(LayoutOptions{Cells: true, Patterns: true, Rect: &Rect{Width: 3, Height: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if l.Mode.Width != 320 || l.Mode.Height != 224 || !l.Mode.H40 || l.Backdrop != [2]int{2, 1} {
		t.Fatalf("mode %+v backdrop %v", l.Mode, l.Backdrop)
	}
	a, b, window := l.Planes[0], l.Planes[1], l.Planes[2]
	if a.Grid[0] != ". 5/2*h ." || a.NonEmpty != 1 || a.Palettes[2] != 1 {
		t.Fatalf("plane A %+v", a)
	}
	if a.Cells[0].X != 1 || a.Cells[0].Tile != 5 || !a.Cells[0].Priority || !a.Cells[0].HFlip || a.Cells[0].Pattern == "" {
		t.Fatalf("plane A cell %+v", a.Cells[0])
	}
	if b.Grid[0] != "7/1 . ." || b.Base != 0xE000 {
		t.Fatalf("plane B %+v", b)
	}
	if window.Grid[0] != "- - -" || window.NonEmpty != 0 {
		t.Fatalf("window %+v", window)
	}
	if l.Scroll.PlaneA.Horizontal[0] != (ScrollRun{From: 0, To: 223, Value: 8}) || l.Scroll.PlaneB.Vertical[0].Value != 8 {
		t.Fatalf("scroll %+v", l.Scroll)
	}
}

func TestLayoutWindowReplacesPlaneA(t *testing.T) {
	s := syntheticSnapshot()
	s.Regs[3] = 0x2C  // window 0xB000
	s.Regs[17] = 0x02 // columns 0..3 use the window
	put(s, 0xB000, 9)
	put(s, 0xC000, 5)
	l, err := s.Layout(LayoutOptions{Planes: []string{"window", "a"}, Rect: &Rect{Width: 5, Height: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if l.Planes[0].Grid[0] != "9/0 . . . -" || l.Planes[1].Grid[0] != "- - - - ." {
		t.Fatalf("window/plane grids %q %q", l.Planes[0].Grid, l.Planes[1].Grid)
	}
}

func TestLayoutFollowsSpriteLinks(t *testing.T) {
	s := syntheticSnapshot()
	base := 0xF000
	put(s, base, 128+10)
	put(s, base+2, 0x0502)      // 2x2 cells, link 2
	put(s, base+4, 0x2000|0x20) // palette 1 tile 0x20
	put(s, base+6, 128+20)
	put(s, base+16, 128+300) // entry 2 below the screen
	put(s, base+18, 0x0000)  // 1x1, end of chain
	l, err := s.Layout(LayoutOptions{Planes: []string{"a"}, Sprites: true, HiddenSprites: true})
	if err != nil {
		t.Fatal(err)
	}
	sp := l.Sprites
	if sp.Linked != 2 || sp.Visible != 1 || len(sp.Entries) != 2 || sp.Limit != 80 {
		t.Fatalf("sprites %+v", sp)
	}
	first := sp.Entries[0]
	if first.Index != 0 || first.X != 20 || first.Y != 10 || first.Width != 2 || first.Height != 2 || first.Tile != 0x20 || first.Palette != 1 || !first.Visible {
		t.Fatalf("first sprite %+v", first)
	}
	if second := sp.Entries[1]; second.Index != 2 || second.Order != 1 || second.Visible {
		t.Fatalf("second sprite %+v", second)
	}
	visibleOnly, _ := s.Layout(LayoutOptions{Planes: []string{"a"}, Sprites: true})
	if len(visibleOnly.Sprites.Entries) != 1 {
		t.Fatalf("hidden sprite returned: %+v", visibleOnly.Sprites)
	}
	put(s, base+18, 0x0000|0) // keep chain end
	put(s, base+2, 0x0500)    // entry 0 links to itself through 0: chain ends immediately
	if l, _ := s.Layout(LayoutOptions{Planes: []string{"a"}, Sprites: true}); l.Sprites.Linked != 1 {
		t.Fatalf("chain should end at link 0: %+v", l.Sprites)
	}
}

func TestLayoutRejectsInvalidRequests(t *testing.T) {
	s := syntheticSnapshot()
	for _, opts := range []LayoutOptions{{Rect: &Rect{X: 40, Width: 1, Height: 1}}, {Rect: &Rect{Width: 41}}, {Planes: []string{"c"}}} {
		if _, err := s.Layout(opts); err == nil {
			t.Fatalf("accepted %+v", opts)
		}
	}
	s.Regs[1] = 0x40
	if _, err := s.Layout(LayoutOptions{}); err == nil || !strings.Contains(err.Error(), "Mode 5") {
		t.Fatalf("Mode 4 accepted: %v", err)
	}
}
