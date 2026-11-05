package lanehook

import (
	"path/filepath"
	"testing"
)

// Saving and re-loading a definition set must preserve the plan a lane resolves
// to. This is the round-trip the API depends on: a PUT that silently drops a
// step would still return 200.
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.toml")

	original, err := LoadFile(filepath.Join("..", "..", "scripts", "hooks", "hooks.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveFile(path, original); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	back, err := LoadFile(path)
	if err != nil {
		t.Fatalf("reload after save: %v", err)
	}
	if len(back.Hooks) != len(original.Hooks) {
		t.Fatalf("step count changed: %d -> %d", len(original.Hooks), len(back.Hooks))
	}
	before, _ := Resolve(original.Hooks, "code-factory-qa", "code-factory", "/w", "/r", original.Order(), nil)
	after, _ := Resolve(back.Hooks, "code-factory-qa", "code-factory", "/w", "/r", back.Order(), nil)
	eq(t, ids(before.Pre), ids(after.Pre))
	eq(t, ids(before.Post), ids(after.Post))
	if spec, ok := back.Lanes["code-factory-qa"]; !ok || spec.Repo != original.Lanes["code-factory-qa"].Repo {
		t.Fatalf("lane spec lost in round trip: %+v", spec)
	}

	// A second save keeps a timestamped backup of the first.
	if err := SaveFile(path, original); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(path + ".bak-*")
	if len(matches) == 0 {
		t.Fatal("SaveFile did not keep a backup")
	}
}

// Upsert replaces an existing key or appends, and refuses an invalid result.
func TestUpsertAndDelete(t *testing.T) {
	f := &File{Lanes: map[string]LaneSpec{}}
	s := Step{ID: "x", Phase: PhasePre, Level: LevelLane, Target: "l", Priority: 10, Command: "/bin/true"}
	if _, err := f.Upsert(s); err != nil {
		t.Fatal(err)
	}
	if len(f.Hooks) != 1 {
		t.Fatalf("want 1 step, got %d", len(f.Hooks))
	}
	s.Priority = 20
	prev, err := f.Upsert(s)
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || prev.Priority != 10 {
		t.Fatalf("want the replaced step back, got %+v", prev)
	}
	if len(f.Hooks) != 1 || f.Hooks[0].Priority != 20 {
		t.Fatalf("replace failed: %+v", f.Hooks)
	}
	// Duplicate id at the SAME key is a replace; an invalid step is refused.
	bad := s
	bad.ID = "y"
	bad.Command = "relative/path"
	if _, err := f.Upsert(bad); err == nil {
		t.Fatal("expected validation error for a relative command")
	}
	if n := f.Delete("x"); n != 1 {
		t.Fatalf("delete removed %d", n)
	}
	if n := f.Delete("nope"); n != 0 {
		t.Fatalf("delete of a missing id removed %d", n)
	}
}

func TestCloneIsIndependent(t *testing.T) {
	f := &File{Hooks: []Step{{ID: "a"}}, Lanes: map[string]LaneSpec{"l": {Repo: "/r"}}}
	c := f.Clone()
	c.Hooks[0].ID = "b"
	c.Lanes["l"] = LaneSpec{Repo: "/other"}
	if f.Hooks[0].ID != "a" || f.Lanes["l"].Repo != "/r" {
		t.Fatal("Clone aliased the original")
	}
}
