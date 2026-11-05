package lanehook

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// saveHeader is written above the encoded body. The API is a legitimate writer
// (S13 §4.2), but an encoded file loses hand-written comments — so the file
// says so rather than pretending it round-tripped the operator's prose.
const saveHeader = `# hooks.toml — S13 lane hook definitions.
#
# WRITTEN BY THE API. This file was last rewritten by PUT /api/v1/hooks/{id};
# comments and ordering from a hand-edited version are not preserved.
# A timestamped backup (hooks.toml.bak-*) is kept beside it.
#
# Edit by hand when comments matter: the daemon reloads on mtime+size change
# and keeps the previous set armed if the new content is invalid.

`

// SaveFile writes f to path atomically, backing up any existing file first.
// The caller must have validated f (Validate) — SaveFile re-checks anyway.
func SaveFile(path string, f *File) error {
	if err := Validate(f.Hooks); err != nil {
		return err
	}
	var body bytes.Buffer
	body.WriteString(saveHeader)
	if err := toml.NewEncoder(&body).Encode(f); err != nil {
		return fmt.Errorf("encode hooks: %w", err)
	}

	if _, err := os.Stat(path); err == nil {
		bak := fmt.Sprintf("%s.bak-%s", path, time.Now().Format("20060102-150405"))
		if data, rerr := os.ReadFile(path); rerr == nil {
			if werr := os.WriteFile(bak, data, 0o644); werr != nil {
				return fmt.Errorf("backup %s: %w", bak, werr)
			}
		}
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".hooks-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(body.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Upsert replaces the step with the same (id, phase, level, target) or appends
// it. It returns the previous definition when one was replaced.
func (f *File) Upsert(s Step) (replaced *Step, err error) {
	if s.Level == LevelGlobal && s.Target == "" {
		s.Target = "*"
	}
	key := func(x Step) string {
		return strings.Join([]string{x.ID, string(x.Phase), string(x.Level), x.Target}, "|")
	}
	for i := range f.Hooks {
		if key(f.Hooks[i]) == key(s) {
			prev := f.Hooks[i]
			f.Hooks[i] = s
			if err := Validate(f.Hooks); err != nil {
				f.Hooks[i] = prev
				return nil, err
			}
			return &prev, nil
		}
	}
	next := append(append([]Step{}, f.Hooks...), s)
	if err := Validate(next); err != nil {
		return nil, err
	}
	f.Hooks = next
	return nil, nil
}

// Delete removes a step by id (all phases/levels carrying that id).
// It returns the number removed.
func (f *File) Delete(id string) int {
	kept := f.Hooks[:0]
	removed := 0
	for _, s := range f.Hooks {
		if s.ID == id {
			removed++
			continue
		}
		kept = append(kept, s)
	}
	f.Hooks = kept
	return removed
}

// Clone returns a deep-enough copy for mutate-then-save flows.
func (f *File) Clone() *File {
	out := &File{Scheduler: f.Scheduler, Hooks: append([]Step{}, f.Hooks...)}
	out.Lanes = make(map[string]LaneSpec, len(f.Lanes))
	for k, v := range f.Lanes {
		out.Lanes[k] = v
	}
	return out
}
