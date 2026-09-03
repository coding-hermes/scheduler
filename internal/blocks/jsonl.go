package blocks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// warnf logs a store warning with the blocks prefix so operators can spot
// torn-file tolerance events in scheduler.log. The scheduler daemon already
// directs log output to its log file, so these surface there.
func warnf(format string, args ...interface{}) {
	log.Printf("blocks: "+format, args...)
}

// loadJSONL reads a JSONL file tolerantly and decodes every well-formed line
// into T. The reader never fails on bad content — that is the point of the
// JSONL-tolerance contract (see the jq-jsonl-tolerant-parsing doctrine):
//
//   - a missing file is an empty list (first boot writes nothing until the
//     first mutation);
//   - blank lines are skipped silently;
//   - a malformed line mid-file is skipped with a logged warning;
//   - a TORN final line (file does not end in '\n', e.g. a crash mid-write by
//     an external tool) is skipped with a logged warning;
//   - duplicate names resolve last-wins in place, with a logged warning
//     (manual edits can duplicate; the newest record is authoritative —
//     same last-row-wins convention as fleet JSONL boards).
//
// key extracts the record's identity (Group.Name / Template.Name) for the
// last-wins dedupe. Records keep their on-disk order (minus skipped lines).
func loadJSONL[T any](path string, key func(*T) string) ([]T, error) {
	var out []T
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return out, nil
	}

	// A file not ending in '\n' has a torn final fragment — a crash or
	// external write interrupted it. Drop the fragment and warn (never fail).
	hasFinalNL := data[len(data)-1] == '\n'
	lines := bytes.Split(data, []byte{'\n'})
	if !hasFinalNL {
		lines = lines[:len(lines)-1] // last element is the torn fragment
		warnf("torn final line in %s — skipped (file does not end in newline)", path)
	}

	seen := map[string]int{} // record name → index in out
	for i, raw := range lines {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var rec T
		if err := json.Unmarshal(line, &rec); err != nil {
			warnf("skipping malformed JSONL line %d in %s: %v", i+1, path, err)
			continue
		}
		name := key(&rec)
		if idx, dup := seen[name]; dup {
			warnf("duplicate %q in %s — keeping the LAST occurrence (line %d)", name, path, i+1)
			out[idx] = rec // last-wins, keeps original position
			continue
		}
		seen[name] = len(out)
		out = append(out, rec)
	}
	return out, nil
}

// writeJSONL atomically replaces path with the given records, one compact
// JSON object per line plus a trailing newline. The write goes to a temp
// file in the same directory, is fsynced, and is renamed over the target —
// readers therefore always see a complete old or new file, never a partial
// write. The parent directory is created on demand so a first boot against a
// fresh data dir works.
func writeJSONL[T any](path string, recs []T) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			cleanup()
			return fmt.Errorf("marshal record: %w", err)
		}
		if _, err := tmp.Write(append(line, '\n')); err != nil {
			cleanup()
			return fmt.Errorf("write temp %s: %w", tmpName, err)
		}
	}
	// 0644 so the config-like JSONL files are group/other readable (backup
	// tooling and other teams pointing at the files should not need sudo).
	if err := tmp.Chmod(0o644); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename %s → %s: %w", tmpName, path, err)
	}
	return nil
}
