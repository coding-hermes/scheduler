package lanehook

import (
	"strings"
	"testing"
)

func TestUnknownPlaceholders(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"known tokens only", "--lane {lane} --project {project} --worktree {workdir}", nil},
		{"one typo", "--worktree {workdirr}", []string{"workdirr"}},
		{"dedupes repeats", "{oops} {oops}", []string{"oops"}},
		{"shell var is not ours", "${HOME}/x", nil},
		{"json snippet is not a placeholder", `{"a":1}`, nil},
		{"no braces", "--plain value", nil},
		{"unterminated brace is ignored", "{lane", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := UnknownPlaceholders(c.in)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("UnknownPlaceholders(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestValidateRejectsUnknownPlaceholder is the load-time half of S13's promise
// that a hook definition cannot quietly send a broken argument: Expand leaves an
// unknown token as literal text, so the hook runs and fails for a reason its
// receipt cannot explain.
func TestValidateRejectsUnknownPlaceholder(t *testing.T) {
	base := Step{
		ID: "wt", Phase: PhasePre, Level: LevelLane, Target: "code-factory-qa",
		Priority: 20, Command: "/usr/bin/true", TimeoutS: 30, OnFailure: FailClosed,
	}

	bad := base
	bad.Args = []string{"--worktree", "{workdirr}"}
	err := Validate([]Step{bad})
	if err == nil {
		t.Fatal("Validate accepted an unknown placeholder in args")
	}
	if !strings.Contains(err.Error(), "{workdirr}") || !strings.Contains(err.Error(), "args[1]") {
		t.Errorf("error = %v, want it to name args[1] and {workdirr}", err)
	}
	if !strings.Contains(err.Error(), "{workdir}") {
		t.Errorf("error = %v, want it to list the known vocabulary so the fix is obvious", err)
	}

	badCmd := base
	badCmd.Command = "/home/kara/.hermes/hooks/{lanes}/run.sh"
	if err := Validate([]Step{badCmd}); err == nil {
		t.Fatal("Validate accepted an unknown placeholder in command")
	}

	good := base
	good.Args = []string{"--lane", "{lane}", "--project", "{project}", "--worktree", "{workdir}"}
	if err := Validate([]Step{good}); err != nil {
		t.Fatalf("Validate rejected a correct definition: %v", err)
	}
}

func TestRenderArgvExpandsAndQuotes(t *testing.T) {
	step := Step{
		ID: "wt", Phase: PhasePre, Level: LevelLane, Target: "code-factory-qa",
		Command: "/home/kara/.hermes/hooks/worktree-ensure.sh",
		Args:    []string{"--lane", "{lane}", "--project", "{project}", "--worktree", "{workdir}"},
	}
	env := Env{
		Lane:    "code-factory-qa",
		Project: "code-factory",
		Role:    "qa",
		Workdir: "/home/kara/proj wt with space/code-factory-qa", // spaces must survive
	}
	got := RenderArgv(step, env)
	if strings.Contains(got, "{") {
		t.Errorf("RenderArgv left a placeholder unexpanded: %q", got)
	}
	if !strings.Contains(got, "--lane code-factory-qa") {
		t.Errorf("RenderArgv = %q, want the lane expanded", got)
	}
	if !strings.Contains(got, "'/home/kara/proj wt with space/code-factory-qa'") {
		t.Errorf("RenderArgv = %q, want a space-bearing path single-quoted so the rendering cannot be misread", got)
	}
	if !strings.HasPrefix(got, step.Command+" ") {
		t.Errorf("RenderArgv = %q, want it to start with the command path", got)
	}
}
