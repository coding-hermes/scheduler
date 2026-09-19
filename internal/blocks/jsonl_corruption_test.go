package blocks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newRNG(seed int64) *testRNG { return &testRNG{v: seed} }

type testRNG struct{ v int64 }

func (r *testRNG) Intn(n int) int {
	r.v = (r.v*1664525 + 1013904223) & ((1 << 63) - 1)
	return int(r.v % int64(n))
}

func (r *testRNG) Int() int {
	n := r.Intn(1 << 31)
	if n < 0 {
		return -n
	}
	return n
}

func jsonMarshalRecords(recs []Group) ([]byte, error) {
	var b bytes.Buffer
	for _, rec := range recs {
		out, err := json.Marshal(rec)
		if err != nil {
			return nil, err
		}
		b.Write(out)
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}

func mutateBytes(r *testRNG, b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	mode := r.Intn(6)
	switch mode {
	case 0:
		pos := r.Intn(len(out) + 1)
		if pos > 0 {
			out = out[:pos]
		}
	case 1:
		pos := r.Intn(len(out))
		out = append(out[:pos], out[pos+1:]...)
	case 2:
		pos := r.Intn(len(out))
		out[pos] ^= 1
	case 3:
		pos := r.Intn(len(out))
		out = append(out[:pos], out[pos+1:]...)
	case 4:
		pos := r.Intn(len(out))
		out = append(out[:pos], out[pos:]...)
	case 5:
		if len(out) > 1 {
			pos := r.Intn(len(out) - 1)
			out = append(out[:pos], out[pos+1:]...)
			out = append(out[:pos], out[pos:]...)
		}
	}
	return out
}

// --- 1. BOM-prefixed file: reader must strip the BOM and decode the first record ---

func TestJSONL_BOMPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.jsonl")
	bom := []byte{0xEF, 0xBB, 0xBF}
	content := append(bom, []byte(`{"name":"bom","projects":["p1"],"description":"first"}`+"\n")...)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := loadJSONL(path, func(g *Group) string { return g.Name })
	if err != nil {
		t.Fatalf("loadJSONL: %v", err)
	}
	if len(got) != 1 || got[0].Name != "bom" {
		t.Fatalf("got %+v, want 1 record named bom", got)
	}
}

// --- 2. Whitespace-only lines: silently skipped ---

func TestJSONL_WhitespaceOnlyLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.jsonl")
	content := []byte(`{"name":"one","projects":[],"description":"first"}
  	
{"name":"two","projects":[],"description":"second"}
`)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := loadJSONL(path, func(g *Group) string { return g.Name })
	if err != nil {
		t.Fatalf("loadJSONL: %v", err)
	}
	if len(got) != 2 || got[0].Name != "one" || got[1].Name != "two" {
		t.Fatalf("got %+v, want [one two]", got)
	}
}

// --- 3. Read-only parent directory: write must fail and leave no .tmp files ---

func TestJSONL_ReadOnlyParentDir(t *testing.T) {
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	path := filepath.Join(ro, "groups.jsonl")
	// First write succeeds and creates the directory.
	if err := writeJSONL(path, []Group{{Name: "x", Projects: []string{"p"}, Description: "d"}}); err != nil {
		t.Fatalf("initial writeJSONL: %v", err)
	}
	// Make the parent directory read-only and retry.
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatalf("chmod ro: %v", err)
	}
	err := writeJSONL(path, []Group{{Name: "y", Projects: []string{"q"}, Description: "e"}})
	if err == nil {
		t.Fatal("writeJSONL into read-only dir succeeded, wanted error")
	}
	entries, err := os.ReadDir(ro)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if bytes.HasPrefix([]byte(e.Name()), []byte(".tmp-")) {
			t.Fatalf("orphaned tmp file left behind: %s", e.Name())
		}
	}
	// Restore writability so t.TempDir cleanup does not fail.
	_ = os.Chmod(ro, 0o755)
}

// --- 4. Round-trip fidelity + order preservation with a 5-record fixture ---

func TestJSONL_RoundTripFiveRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.jsonl")
	orig := []Group{
		{Name: "g01", Projects: []string{"p1"}, Description: "first"},
		{Name: "g02", Projects: []string{"p2"}, Description: "second"},
		{Name: "g03", Projects: []string{"p3"}, Description: "third"},
		{Name: "g04", Projects: []string{"p4"}, Description: "fourth"},
		{Name: "g05", Projects: []string{"p5"}, Description: "fifth"},
	}
	if err := writeJSONL(path, orig); err != nil {
		t.Fatalf("writeJSONL: %v", err)
	}
	got, err := loadJSONL(path, func(g *Group) string { return g.Name })
	if err != nil {
		t.Fatalf("loadJSONL: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("len = %d, want 5", len(got))
	}
	origB, err := jsonMarshalRecords(orig)
	if err != nil {
		t.Fatalf("marshal orig: %v", err)
	}
	gotB, err := jsonMarshalRecords(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	if !bytes.Equal(origB, gotB) {
		t.Fatalf("round-trip bytes differ:\norig:\n%s\ngot:\n%s", origB, gotB)
	}
	for i := range orig {
		if got[i].Name != orig[i].Name {
			t.Fatalf("order changed at %d: got %s, want %s", i, got[i].Name, orig[i].Name)
		}
	}
}

// --- 5. Seeded pseudo-fuzz over the same 5-record fixture ---

func TestJSONL_SeededPseudoFuzz(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.jsonl")
	base := []Group{
		{Name: "g01", Projects: []string{"p1"}, Description: "first"},
		{Name: "g02", Projects: []string{"p2"}, Description: "second"},
		{Name: "g03", Projects: []string{"p3"}, Description: "third"},
		{Name: "g04", Projects: []string{"p4"}, Description: "fourth"},
		{Name: "g05", Projects: []string{"p5"}, Description: "fifth"},
	}
	if err := writeJSONL(path, base); err != nil {
		t.Fatalf("writeJSONL base: %v", err)
	}
	r := newRNG(0xC0FFEE)
	for iter := 0; iter < 50; iter++ {
		b, _ := os.ReadFile(path)
		mut := mutateBytes(r, b)
		if err := os.WriteFile(path, mut, 0o644); err != nil {
			t.Fatalf("iter %d write: %v", iter, err)
		}
		_, err := loadJSONL(path, func(g *Group) string { return g.Name })
		if err != nil {
			t.Fatalf("iter %d loadJSONL: %v", iter, err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("iter %d ReadDir: %v", iter, err)
		}
		for _, e := range entries {
			if bytes.HasPrefix([]byte(e.Name()), []byte(".tmp-")) {
				t.Fatalf("iter %d: orphan tmp file %s", iter, e.Name())
			}
		}
	}
}
