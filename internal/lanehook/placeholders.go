package lanehook

import "strings"

// knownPlaceholders is the vocabulary Expand() substitutes (resolve.go). It is
// the single source of truth for "what a hook command may say"; the validator
// and the CLI preview both read it from here.
var knownPlaceholders = []string{
	"lane", "project", "role", "namespace", "workdir", "repo", "tick", "phase", "hook",
}

// KnownPlaceholderList renders the vocabulary for an error message.
func KnownPlaceholderList() string {
	parts := make([]string, len(knownPlaceholders))
	for i, p := range knownPlaceholders {
		parts[i] = "{" + p + "}"
	}
	return strings.Join(parts, " ")
}

func isKnownPlaceholder(tok string) bool {
	for _, p := range knownPlaceholders {
		if tok == p {
			return true
		}
	}
	return false
}

// UnknownPlaceholders returns every {...} token in s that Expand cannot fill,
// in the order they appear, without duplicates.
//
// Why this exists: Expand replaces the tokens it knows and silently leaves the
// rest as literal text. A typo ({workdirr}, {lane_name}) therefore reaches the
// exec'd command as a literal string, the hook exits non-zero for a reason the
// receipt cannot explain, and the operator debugs a script that is fine. The
// loader refuses the definition instead. Literal braces are not supported in
// command/args for exactly this reason: an unescaped '{...}' is always read as
// an attempted placeholder.
func UnknownPlaceholders(s string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		rel := strings.IndexByte(s[i:], '}')
		if rel < 0 {
			break
		}
		tok := s[i+1 : i+rel]
		i += rel
		// Only a bare lowercase-word token can be one of ours. That single rule
		// keeps shell ${HOME} (uppercase), JSON snippets ({"a":1}) and stray
		// braces in prose out of the check without a growing list of
		// exceptions — Expand leaves every one of those untouched anyway, so
		// flagging them would be a false positive that teaches operators to
		// bypass the validator.
		if !isPlaceholderToken(tok) {
			continue
		}
		if !isKnownPlaceholder(tok) && !seen[tok] {
			seen[tok] = true
			out = append(out, tok)
		}
	}
	return out
}

func quoteBraced(toks []string) string {
	parts := make([]string, len(toks))
	for i, t := range toks {
		parts[i] = "{" + t + "}"
	}
	return strings.Join(parts, ", ")
}

// isPlaceholderToken reports whether tok has the shape of a placeholder name:
// non-empty, lowercase ASCII letters and underscores only.
func isPlaceholderToken(tok string) bool {
	if tok == "" {
		return false
	}
	for _, r := range tok {
		if (r < 'a' || r > 'z') && r != '_' {
			return false
		}
	}
	return true
}

// RenderArgv is the command line a step will actually run, expanded for env,
// as one readable string for receipts and the CLI preview. Arguments that need
// it are single-quoted so the rendering cannot be mistaken for a different
// argv than the one exec receives (exec gets the slice, never this string).
func RenderArgv(step Step, env Env) string {
	argv := append([]string{step.Command}, Expand(step.Args, env)...)
	for i, a := range argv {
		if i == 0 {
			continue // the command path is our own absolute path, no quoting
		}
		if a == "" || strings.ContainsAny(a, " 	'\"") {
			argv[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(argv, " ")
}
