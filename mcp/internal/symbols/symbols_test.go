package symbols

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadResolveLocate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "symbols.txt")
	data := "00000200 T reset\n0000027a t main_loop\n00ff0004 B frame_counter\nffffffff junk line here extra\nnot-hex B skipped\n00ff0008 input_state\n00ff0004 B frame_counter\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	table, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if table.Len() != 4 {
		t.Fatalf("len %d", table.Len())
	}
	for expr, want := range map[string]uint32{"frame_counter": 0xFF0004, "frame_counter+4": 0xFF0008, "input_state - 0x2": 0xFF0006, "main_loop+0x10": 0x28A} {
		if got, err := table.Resolve(expr); err != nil || got != want {
			t.Fatalf("%s = %x, %v", expr, got, err)
		}
	}
	for _, bad := range []string{"missing", "reset+x", "reset-0x300"} {
		if _, err := table.Resolve(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if table.Locate(0x27A) != "main_loop" || table.Locate(0x290) != "main_loop+0x16" || table.Locate(0x100) != "" {
		t.Fatalf("locate: %q %q", table.Locate(0x27A), table.Locate(0x290))
	}
	if found := table.Find("_", 10); len(found) != 3 {
		t.Fatalf("find: %+v", found)
	}
	var none *Table
	if _, err := none.Resolve("x"); err == nil || none.Locate(1) != "" {
		t.Fatal("nil table must reject lookups")
	}
}
