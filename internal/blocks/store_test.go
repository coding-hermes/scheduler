package blocks

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	return NewStore(filepath.Join(dir, "groups.jsonl"), filepath.Join(dir, "templates.jsonl"))
}

func testGroup(name string) Group {
	return Group{Name: name, Projects: []string{"alpha", "beta"}, Description: "test group " + name}
}

func testTemplate(name string) Template {
	return Template{
		Name:        name,
		Description: "test template " + name,
		Tasks: []TemplateTask{
			{Title: "Audit {PROJECT} board hygiene", Detail: "Check the board", Labels: []string{"audit"}},
			{Title: "Close stale rows on {PROJECT}"},
		},
	}
}

// --- groups: CRUD round trip ---

func TestStoreGroupsCRUD(t *testing.T) {
	s := newTestStore(t)

	// Empty store lists nothing and Get misses.
	groups, err := s.ListGroups()
	if err != nil {
		t.Fatalf("ListGroups (empty): %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("empty store returned %d groups", len(groups))
	}
	if _, err := s.GetGroup("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetGroup(missing) err = %v, want ErrNotFound", err)
	}

	if err := s.CreateGroup(testGroup("b-group")); err != nil {
		t.Fatalf("CreateGroup b: %v", err)
	}
	if err := s.CreateGroup(testGroup("a-group")); err != nil {
		t.Fatalf("CreateGroup a: %v", err)
	}
	// Duplicate create → ErrExists.
	if err := s.CreateGroup(testGroup("a-group")); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate CreateGroup err = %v, want ErrExists", err)
	}
	// Validation: empty name.
	if err := s.CreateGroup(Group{}); err == nil {
		t.Fatal("CreateGroup with empty name should fail validation")
	}

	groups, err = s.ListGroups()
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("len(groups) = %d, want 2", len(groups))
	}
	// Sorted by name.
	if groups[0].Name != "a-group" || groups[1].Name != "b-group" {
		t.Errorf("list order = [%s %s], want [a-group b-group]", groups[0].Name, groups[1].Name)
	}

	g, err := s.GetGroup("a-group")
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if len(g.Projects) != 2 || g.Projects[0] != "alpha" {
		t.Errorf("GetGroup projects = %v, want [alpha beta]", g.Projects)
	}

	// Update (partial pointer semantics).
	desc := "renamed purpose"
	projects := []string{"gamma"}
	updated, err := s.UpdateGroup("a-group", GroupUpdate{Projects: &projects, Description: &desc})
	if err != nil {
		t.Fatalf("UpdateGroup: %v", err)
	}
	if updated.Description != desc || len(updated.Projects) != 1 || updated.Projects[0] != "gamma" {
		t.Errorf("updated group = %+v", updated)
	}
	got, _ := s.GetGroup("a-group")
	if got.Description != desc {
		t.Errorf("persisted description = %q, want %q", got.Description, desc)
	}
	// Update missing → ErrNotFound.
	if _, err := s.UpdateGroup("nope", GroupUpdate{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateGroup(missing) err = %v, want ErrNotFound", err)
	}

	// Delete.
	if err := s.DeleteGroup("a-group"); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	if _, err := s.GetGroup("a-group"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetGroup after delete err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteGroup("a-group"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteGroup(missing) err = %v, want ErrNotFound", err)
	}
	groups, _ = s.ListGroups()
	if len(groups) != 1 {
		t.Fatalf("after delete len(groups) = %d, want 1", len(groups))
	}
}

// --- templates: CRUD + validation ---

func TestStoreTemplatesCRUD(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateTemplate(testTemplate("tpl-a")); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if err := s.CreateTemplate(testTemplate("tpl-b")); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if err := s.CreateTemplate(testTemplate("tpl-a")); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate CreateTemplate err = %v, want ErrExists", err)
	}
	// A template needs at least one task with a title.
	if err := s.CreateTemplate(Template{Name: "empty", Tasks: nil}); err == nil {
		t.Error("template with no tasks should fail validation")
	}
	if err := s.CreateTemplate(Template{Name: "bad", Tasks: []TemplateTask{{Title: "  "}}}); err == nil {
		t.Error("template with blank task title should fail validation")
	}

	templates, err := s.ListTemplates()
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(templates) != 2 {
		t.Fatalf("len(templates) = %d, want 2", len(templates))
	}
	if templates[0].Name != "tpl-a" {
		t.Errorf("list[0].Name = %s, want tpl-a (sorted)", templates[0].Name)
	}

	// Update replaces the task list.
	tasks := []TemplateTask{{Title: "only task", Labels: []string{"x"}}}
	updated, err := s.UpdateTemplate("tpl-b", TemplateUpdate{Tasks: &tasks})
	if err != nil {
		t.Fatalf("UpdateTemplate: %v", err)
	}
	if len(updated.Tasks) != 1 || updated.Tasks[0].Title != "only task" {
		t.Errorf("updated template = %+v", updated)
	}
	if _, err := s.UpdateTemplate("nope", TemplateUpdate{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateTemplate(missing) err = %v, want ErrNotFound", err)
	}

	if err := s.DeleteTemplate("tpl-a"); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}
	if _, err := s.GetTemplate("tpl-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTemplate after delete err = %v, want ErrNotFound", err)
	}
}

// --- tolerant reading ---

func writeBoardFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestStoreTornLastLineTolerated(t *testing.T) {
	s := newTestStore(t)
	// Valid records followed by a torn final fragment (no trailing newline) —
	// the crash-mid-write signature. The reader must return the valid
	// records and warn, never fail.
	content := `{"name":"one","projects":["p1"],"description":"first"}
{"name":"two","projects":["p2"],"description":"second"}
{"name":"three","projects":["p3"],"descrip`
	writeBoardFile(t, s.GroupsPath(), content)

	groups, err := s.ListGroups()
	if err != nil {
		t.Fatalf("ListGroups on torn file: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("torn file returned %d groups, want 2 (torn fragment skipped)", len(groups))
	}
	if groups[0].Name != "one" || groups[1].Name != "two" {
		t.Errorf("groups = %+v", groups)
	}
}

func TestStoreMalformedMiddleLineSkipped(t *testing.T) {
	s := newTestStore(t)
	content := `{"name":"one","projects":[],"description":""}
this is not json
{"name":"two","projects":[],"description":""}
`
	writeBoardFile(t, s.GroupsPath(), content)
	writeBoardFile(t, s.TemplatesPath(), content)
	groups, err := s.ListGroups()
	if err != nil {
		t.Fatalf("ListGroups with garbage line: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("garbage line: got %d groups, want 2 (skipped)", len(groups))
	}
	templates, err := s.ListTemplates()
	if err != nil {
		t.Fatalf("ListTemplates with garbage line: %v", err)
	}
	if len(templates) != 2 {
		t.Fatalf("garbage line: got %d templates, want 2 (skipped)", len(templates))
	}
}

func TestStoreDuplicateNameLastWins(t *testing.T) {
	s := newTestStore(t)
	content := `{"name":"dup","projects":["old"],"description":"old version"}
{"name":"dup","projects":["new"],"description":"new version"}
`
	writeBoardFile(t, s.GroupsPath(), content)
	groups, err := s.ListGroups()
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("duplicate names: got %d groups, want 1 (last-wins)", len(groups))
	}
	if len(groups[0].Projects) != 1 || groups[0].Projects[0] != "new" {
		t.Errorf("last-wins record = %+v, want the 'new' projects", groups[0])
	}
}

func TestStoreMissingFileIsEmpty(t *testing.T) {
	s := newTestStore(t)
	groups, err := s.ListGroups()
	if err != nil || len(groups) != 0 {
		t.Fatalf("missing file: groups=%v err=%v, want empty+nil", groups, err)
	}
	// First mutation creates the file atomically.
	if err := s.CreateGroup(testGroup("first")); err != nil {
		t.Fatalf("CreateGroup on fresh store: %v", err)
	}
	if _, err := os.Stat(s.GroupsPath()); err != nil {
		t.Fatalf("groups.jsonl not created: %v", err)
	}
}

func TestStoreFileRoundTripJSONLShape(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateGroup(testGroup("rt")); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := s.CreateTemplate(testTemplate("rt")); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	// Every line of both files must parse as a standalone JSON object.
	for _, path := range []string{s.GroupsPath(), s.TemplatesPath()} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if len(data) == 0 || data[len(data)-1] != '\n' {
			t.Errorf("%s must end with newline", path)
		}
	}
}
