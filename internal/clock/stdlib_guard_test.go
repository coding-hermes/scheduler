package clock

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// stdlibClockCall matches a direct stdlib clock read or wait. time.Parse,
// time.Duration arithmetic and time.Date construction are NOT clock reads and
// are deliberately not matched.
var stdlibClockCall = regexp.MustCompile(`\btime\.(Now|Since|Until|Sleep|NewTicker|After|AfterFunc)\s*\(`)

// TestNoDirectStdlibClockCallsOutsideClock is the anti-rot guard for the single
// time choke point: non-test sources under internal/ and cmd/ may not read the
// wall clock except inside this package. It mirrors ADV-R10's
// no-second-ceiling guard — without it the seam rots the first time a new
// codepath reaches for time.Now().
//
// The allowlist is exactly one directory (this package). Comment lines are
// ignored (prose that names the stdlib call is documentation, not a read) but
// are reported in the test log so an operator can see them.
func TestNoDirectStdlibClockCallsOutsideClock(t *testing.T) {
	root := repoRoot(t)
	selfDir := filepath.Join(root, "internal", "clock")

	var offenders []string
	var comments []string

	for _, top := range []string{"internal", "cmd"} {
		base := filepath.Join(root, top)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// Testdata is fixture material, never compiled into the daemon.
				if d.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if filepath.Dir(path) == selfDir {
				return nil
			}
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			rel, _ := filepath.Rel(root, path)
			for i, line := range strings.Split(string(body), "\n") {
				if !stdlibClockCall.MatchString(line) {
					continue
				}
				loc := rel + ":" + itoa(i+1) + ": " + strings.TrimSpace(line)
				if isCommentLine(line) {
					comments = append(comments, loc)
					continue
				}
				offenders = append(offenders, loc)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}

	if len(comments) > 0 {
		t.Logf("commented mentions of the stdlib clock (%d) — allowed, prose only:\n%s",
			len(comments), strings.Join(comments, "\n"))
	}
	if len(offenders) > 0 {
		t.Fatalf("%d direct stdlib clock call(s) outside internal/clock — route them through a clock.Clock "+
			"(inject one, or take clock.Real() as an explicit default):\n%s",
			len(offenders), strings.Join(offenders, "\n"))
	}
}

func isCommentLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// repoRoot walks up from the package directory until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the module root (go.mod) above the test directory")
	return ""
}
