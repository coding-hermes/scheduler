package blocks

import (
	"fmt"
	"sort"
	"sync"
)

// Store manages the two JSONL-backed collections: deploy groups
// (groups.jsonl) and deploy templates (templates.jsonl). All mutations are
// serialized through a mutex and persisted with an atomic temp+rename write
// (see jsonl.go). Reads re-parse the file on every call, so they are always
// consistent without holding the lock.
type Store struct {
	groupsPath    string
	templatesPath string

	mu sync.Mutex // serializes read-modify-write mutations on both files
}

// NewStore creates a Store backed by the given JSONL file paths. The paths
// are NOT touched until the first read or write, so constructing a Store
// never fails and never creates files.
func NewStore(groupsPath, templatesPath string) *Store {
	return &Store{
		groupsPath:    groupsPath,
		templatesPath: templatesPath,
	}
}

// GroupsPath returns the JSONL file backing the groups collection.
func (s *Store) GroupsPath() string { return s.groupsPath }

// TemplatesPath returns the JSONL file backing the templates collection.
func (s *Store) TemplatesPath() string { return s.templatesPath }

// ── Groups ────────────────────────────────────────────────────────────────

// ListGroups returns every group in file order (deduped last-wins), sorted
// by name for stable API output. A missing file yields an empty list.
func (s *Store) ListGroups() ([]Group, error) {
	groups, err := loadJSONL(s.groupsPath, func(g *Group) string { return g.Name })
	if err != nil {
		return nil, err
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	return groups, nil
}

// GetGroup returns the named group, or ErrNotFound.
func (s *Store) GetGroup(name string) (Group, error) {
	groups, err := s.ListGroups()
	if err != nil {
		return Group{}, err
	}
	for _, g := range groups {
		if g.Name == name {
			return g, nil
		}
	}
	return Group{}, fmt.Errorf("group %q: %w", name, ErrNotFound)
}

// CreateGroup appends a new group. Duplicate names return ErrExists.
func (s *Store) CreateGroup(g Group) error {
	if err := ValidateGroup(g); err != nil {
		return err
	}
	if g.Projects == nil {
		g.Projects = []string{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	groups, err := loadJSONL(s.groupsPath, func(gr *Group) string { return gr.Name })
	if err != nil {
		return err
	}
	for _, existing := range groups {
		if existing.Name == g.Name {
			return fmt.Errorf("group %q: %w", g.Name, ErrExists)
		}
	}
	groups = append(groups, g)
	return writeJSONL(s.groupsPath, groups)
}

// UpdateGroup applies a partial update to the named group and returns the
// updated record. Unknown names return ErrNotFound.
func (s *Store) UpdateGroup(name string, patch GroupUpdate) (Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	groups, err := loadJSONL(s.groupsPath, func(gr *Group) string { return gr.Name })
	if err != nil {
		return Group{}, err
	}
	for i := range groups {
		if groups[i].Name != name {
			continue
		}
		if patch.Projects != nil {
			groups[i].Projects = *patch.Projects
		}
		if groups[i].Projects == nil {
			groups[i].Projects = []string{}
		}
		if patch.Description != nil {
			groups[i].Description = *patch.Description
		}
		if err := ValidateGroup(groups[i]); err != nil {
			return Group{}, err
		}
		if err := writeJSONL(s.groupsPath, groups); err != nil {
			return Group{}, err
		}
		return groups[i], nil
	}
	return Group{}, fmt.Errorf("group %q: %w", name, ErrNotFound)
}

// DeleteGroup removes the named group. Unknown names return ErrNotFound.
func (s *Store) DeleteGroup(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	groups, err := loadJSONL(s.groupsPath, func(gr *Group) string { return gr.Name })
	if err != nil {
		return err
	}
	kept := groups[:0]
	found := false
	for _, g := range groups {
		if g.Name == name {
			found = true
			continue
		}
		kept = append(kept, g)
	}
	if !found {
		return fmt.Errorf("group %q: %w", name, ErrNotFound)
	}
	return writeJSONL(s.groupsPath, kept)
}

// ── Templates ─────────────────────────────────────────────────────────────

// ListTemplates returns every template sorted by name. A missing file yields
// an empty list.
func (s *Store) ListTemplates() ([]Template, error) {
	templates, err := loadJSONL(s.templatesPath, func(t *Template) string { return t.Name })
	if err != nil {
		return nil, err
	}
	sort.Slice(templates, func(i, j int) bool { return templates[i].Name < templates[j].Name })
	return templates, nil
}

// GetTemplate returns the named template, or ErrNotFound.
func (s *Store) GetTemplate(name string) (Template, error) {
	templates, err := s.ListTemplates()
	if err != nil {
		return Template{}, err
	}
	for _, t := range templates {
		if t.Name == name {
			return t, nil
		}
	}
	return Template{}, fmt.Errorf("template %q: %w", name, ErrNotFound)
}

// CreateTemplate appends a new template. Duplicate names return ErrExists.
func (s *Store) CreateTemplate(t Template) error {
	if err := ValidateTemplate(t); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	templates, err := loadJSONL(s.templatesPath, func(tm *Template) string { return tm.Name })
	if err != nil {
		return err
	}
	for _, existing := range templates {
		if existing.Name == t.Name {
			return fmt.Errorf("template %q: %w", t.Name, ErrExists)
		}
	}
	templates = append(templates, t)
	return writeJSONL(s.templatesPath, templates)
}

// UpdateTemplate applies a partial update to the named template and returns
// the updated record. Unknown names return ErrNotFound.
func (s *Store) UpdateTemplate(name string, patch TemplateUpdate) (Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	templates, err := loadJSONL(s.templatesPath, func(t *Template) string { return t.Name })
	if err != nil {
		return Template{}, err
	}
	for i := range templates {
		if templates[i].Name != name {
			continue
		}
		if patch.Description != nil {
			templates[i].Description = *patch.Description
		}
		if patch.Tasks != nil {
			templates[i].Tasks = *patch.Tasks
		}
		if err := ValidateTemplate(templates[i]); err != nil {
			return Template{}, err
		}
		if err := writeJSONL(s.templatesPath, templates); err != nil {
			return Template{}, err
		}
		return templates[i], nil
	}
	return Template{}, fmt.Errorf("template %q: %w", name, ErrNotFound)
}

// DeleteTemplate removes the named template. Unknown names return ErrNotFound.
func (s *Store) DeleteTemplate(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	templates, err := loadJSONL(s.templatesPath, func(t *Template) string { return t.Name })
	if err != nil {
		return err
	}
	kept := templates[:0]
	found := false
	for _, t := range templates {
		if t.Name == name {
			found = true
			continue
		}
		kept = append(kept, t)
	}
	if !found {
		return fmt.Errorf("template %q: %w", name, ErrNotFound)
	}
	return writeJSONL(s.templatesPath, kept)
}
