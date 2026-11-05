package main

// hooks_cmd.go — the operator surface for lane hooks (S13/S14).
//
// The daemon has had /api/v1/hooks and /api/v1/hooks/plan for a while, but both
// answer only from a RUNNING daemon. The question an operator actually asks —
// "what command will this lane run, exactly, before it spawns?" — had no answer
// on a box where the scheduler is stopped, and none at all before the first
// tick ever fired. That is why the whole feature could sit armed-but-unproven
// without anyone noticing.
//
//	schedulerd hooks preview --lane code-factory-qa
//	schedulerd hooks preview --lane code-factory-qa --tick T-123 --json
//
// It resolves the SAME way the runner does (lanehook.Resolve via Set.PlanFor,
// then Expand), prints the argv that would be exec'd — placeholders already
// substituted — and never executes anything.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/coding-hermes/scheduler/internal/lanehook"
)

type previewStep struct {
	Seq       int    `json:"seq"`
	ID        string `json:"id"`
	Phase     string `json:"phase"`
	Level     string `json:"level"`
	Target    string `json:"target"`
	Priority  int    `json:"priority"`
	Command   string `json:"command"`
	Argv      string `json:"argv"`
	TimeoutS  int    `json:"timeout_s"`
	OnFailure string `json:"on_failure"`
	Enabled   bool   `json:"enabled"`
}

type previewDoc struct {
	Lane      string        `json:"lane"`
	Project   string        `json:"project"`
	Role      string        `json:"role"`
	Branch    string        `json:"branch"`
	HooksFile string        `json:"hooks_file"`
	TickID    string        `json:"tick_id"`
	RealTick  bool          `json:"real_tick"`
	Workdir   string        `json:"workdir"`
	Repo      string        `json:"repo"`
	Executes  bool          `json:"executes"` // always false: a preview never runs
	Pre       []previewStep `json:"pre"`
	Post      []previewStep `json:"post"`
	Warnings  []string      `json:"warnings,omitempty"`
}

func runHooksCommand(args []string) int {
	if len(args) == 0 || args[0] != "preview" {
		fmt.Fprintf(os.Stderr, "usage: schedulerd hooks preview --lane <lane> [--tick <id>] [--workdir DIR] [--repo DIR] [--namespace NS] [--file PATH] [--json]\n")
		return 2
	}
	fs := flag.NewFlagSet("hooks preview", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	file := fs.String("file", os.ExpandEnv("$HOME/.hermes/hooks.toml"), "hooks definition file (the daemon's own --hooks-file)")
	lane := fs.String("lane", "", "lane name to preview, e.g. code-factory-qa")
	tick := fs.String("tick", "", "tick id to substitute for {tick}; omit to preview with a visible placeholder")
	namespace := fs.String("namespace", "", "namespace id for {namespace} and namespace-level hooks")
	workdir := fs.String("workdir", "", "worktree path for {workdir} (empty = leave it empty and warn)")
	repo := fs.String("repo", "", "source repository path for {repo} (empty = leave it empty and warn)")
	asJSON := fs.Bool("json", false, "emit the resolved plan as JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if strings.TrimSpace(*lane) == "" {
		fmt.Fprintln(os.Stderr, "hooks preview: --lane is required (a preview with no lane answers nothing)")
		return 2
	}

	set, err := lanehook.Open(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hooks preview: %s rejected: %v\n", *file, err)
		return 1
	}

	project, role := lanehook.RoleFor(*lane)
	spec, haveSpec := set.Lane(*lane)
	skip := []string{}
	if haveSpec {
		skip = spec.SkipHooks
	}

	plan, err := set.PlanFor(*lane, *namespace, *workdir, *repo, skip)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hooks preview: resolve %s: %v\n", *lane, err)
		return 1
	}

	tickID, realTick := *tick, *tick != ""
	if tickID == "" {
		tickID = "<tick>"
	}
	env := lanehook.Env{
		TickID:    tickID,
		Lane:      *lane,
		Project:   project,
		Role:      role,
		Namespace: *namespace,
		Workdir:   *workdir,
		Repo:      *repo,
	}

	doc := previewDoc{
		Lane:      *lane,
		Project:   project,
		Role:      role,
		Branch:    fmt.Sprintf("lane/%s/%s", project, role),
		HooksFile: *file,
		TickID:    tickID,
		RealTick:  realTick,
		Workdir:   *workdir,
		Repo:      *repo,
		Executes:  false,
		Pre:       renderSteps(plan.Pre, lanehook.PhasePre, env),
		Post:      renderSteps(plan.Post, lanehook.PhasePost, env),
	}
	if *workdir == "" {
		doc.Warnings = append(doc.Warnings, "{workdir} is empty — pass --workdir for an argv that matches a real tick")
	}
	if *repo == "" {
		doc.Warnings = append(doc.Warnings, "{repo} is empty — pass --repo for an argv that matches a real tick")
	}
	if !realTick {
		doc.Warnings = append(doc.Warnings, "{tick} is the literal <tick>: this preview shows the shape, not a specific tick")
	}
	if !haveSpec {
		doc.Warnings = append(doc.Warnings, fmt.Sprintf("no [lane.%q] block in %s — lane-scoped worktree policy (base/policy/push/floor_gib) is unset", *lane, *file))
	}
	if len(plan.Pre) == 0 && len(plan.Post) == 0 {
		doc.Warnings = append(doc.Warnings, "the plan is EMPTY: no hook targets this lane, so a tick would spawn with no pre/post steps")
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(doc); err != nil {
			fmt.Fprintf(os.Stderr, "hooks preview: %v\n", err)
			return 1
		}
		return 0
	}
	printPreview(doc, spec, haveSpec)
	return 0
}

func renderSteps(steps []lanehook.Step, phase lanehook.Phase, env lanehook.Env) []previewStep {
	out := make([]previewStep, 0, len(steps))
	for i, s := range steps {
		e := env
		e.Phase = phase
		e.HookID = s.ID
		out = append(out, previewStep{
			Seq:       i + 1,
			ID:        s.ID,
			Phase:     string(s.Phase),
			Level:     string(s.Level),
			Target:    s.Target,
			Priority:  s.Priority,
			Command:   s.Command,
			Argv:      lanehook.RenderArgv(s, e),
			TimeoutS:  int(s.Timeout().Seconds()),
			OnFailure: string(s.OnFailureOrDefault()),
			Enabled:   s.IsEnabled(),
		})
	}
	return out
}

func printPreview(doc previewDoc, spec lanehook.LaneSpec, haveSpec bool) {
	fmt.Printf("lane            %s\n", doc.Lane)
	fmt.Printf("project / role  %s / %s\n", doc.Project, doc.Role)
	fmt.Printf("branch          %s\n", doc.Branch)
	fmt.Printf("hooks file      %s\n", doc.HooksFile)
	fmt.Printf("tick            %s\n", doc.TickID)
	if haveSpec {
		policy := spec.Policy
		if policy == "" {
			policy = "persistent"
		}
		base := spec.Base
		if base == "" {
			base = "main"
		}
		fmt.Printf("lane spec       repo=%s base=%s policy=%s push=%v floor_gib=%.1f\n",
			spec.Repo, base, policy, spec.Push, spec.FloorGiB)
		if len(spec.SkipHooks) > 0 {
			fmt.Printf("skip_hooks      %s\n", strings.Join(spec.SkipHooks, ", "))
		}
	} else {
		fmt.Printf("lane spec       (none declared)\n")
	}
	fmt.Printf("executes        no — this is a preview\n\n")

	printPhase := func(name string, steps []previewStep) {
		fmt.Printf("%s (%d step(s))\n", name, len(steps))
		if len(steps) == 0 {
			fmt.Printf("  (none)\n\n")
			return
		}
		for _, s := range steps {
			disabled := ""
			if !s.Enabled {
				disabled = " DISABLED"
			}
			fmt.Printf("  [%d] %s  level=%s priority=%d timeout=%ds on_failure=%s%s\n",
				s.Seq, s.ID, s.Level, s.Priority, s.TimeoutS, s.OnFailure, disabled)
			fmt.Printf("      %s\n", s.Argv)
		}
		fmt.Println()
	}
	printPhase("PRE", doc.Pre)
	printPhase("POST", doc.Post)

	for _, w := range doc.Warnings {
		fmt.Printf("warning: %s\n", w)
	}
}
