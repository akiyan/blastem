// Package symbols loads `nm -n` style symbol files so tools can address memory by name.
package symbols

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

type Symbol struct {
	Name    string `json:"name"`
	Address uint32 `json:"address"`
	Type    string `json:"type,omitempty" jsonschema:"nm symbol type letter when present"`
}

type Table struct {
	Path    string
	byName  map[string]Symbol
	ordered []Symbol
}

// Load reads lines of "ADDRESS TYPE NAME" (nm) or "ADDRESS NAME".
func Load(path string) (*Table, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	t := &Table{Path: path, byName: map[string]Symbol{}}
	scanner := bufio.NewScanner(file)
	line := 0
	for scanner.Scan() {
		line++
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || len(fields) > 3 {
			continue
		}
		value, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(fields[0]), "0x"), 16, 32)
		if err != nil {
			continue
		}
		s := Symbol{Name: fields[len(fields)-1], Address: uint32(value)}
		if len(fields) == 3 {
			s.Type = fields[1]
		}
		if _, duplicate := t.byName[s.Name]; duplicate {
			continue // keep the first definition, like nm -n ordering
		}
		t.byName[s.Name] = s
		t.ordered = append(t.ordered, s)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(t.ordered) == 0 {
		return nil, fmt.Errorf("%s: no symbols found (expected nm -n style lines)", path)
	}
	sort.SliceStable(t.ordered, func(i, j int) bool { return t.ordered[i].Address < t.ordered[j].Address })
	return t, nil
}

func (t *Table) Len() int { return len(t.ordered) }

// Resolve accepts NAME, NAME+OFFSET, or NAME-OFFSET (decimal or 0x hex).
func (t *Table) Resolve(expr string) (uint32, error) {
	if t == nil {
		return 0, fmt.Errorf("no symbol file loaded; pass symbols_path to blastem_start")
	}
	name, offset := strings.TrimSpace(expr), int64(0)
	if i := strings.LastIndexAny(name, "+-"); i > 0 {
		n, err := strconv.ParseInt(strings.TrimSpace(name[i+1:]), 0, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid offset in %q", expr)
		}
		if name[i] == '-' {
			n = -n
		}
		name, offset = strings.TrimSpace(name[:i]), n
	}
	s, ok := t.byName[name]
	if !ok {
		return 0, fmt.Errorf("unknown symbol %q", name)
	}
	address := int64(s.Address) + offset
	if address < 0 || address > 0xFFFFFFFF {
		return 0, fmt.Errorf("%q is out of range", expr)
	}
	return uint32(address), nil
}

// Locate names an address as the closest symbol at or below it, e.g. "main_loop+0x12".
func (t *Table) Locate(address uint32) string {
	if t == nil {
		return ""
	}
	i := sort.Search(len(t.ordered), func(i int) bool { return t.ordered[i].Address > address }) - 1
	if i < 0 {
		return ""
	}
	s := t.ordered[i]
	if s.Address == address {
		return s.Name
	}
	return fmt.Sprintf("%s+0x%x", s.Name, address-s.Address)
}

// Find returns symbols whose name contains the query, at most limit.
func (t *Table) Find(query string, limit int) []Symbol {
	var out []Symbol
	if t == nil {
		return out
	}
	for _, s := range t.ordered {
		if strings.Contains(s.Name, query) {
			out = append(out, s)
			if len(out) == limit {
				break
			}
		}
	}
	return out
}
