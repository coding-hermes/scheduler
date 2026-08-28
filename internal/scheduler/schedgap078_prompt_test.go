package scheduler

import (
	"strings"
	"testing"
)

// buildForemanPrompt tests (Bane 2026-08-27): namespace default_prompt +
// per-project prompt with append/replace semantics. The dynamic footer
// (tick id, workdir, worker model/provider) must ALWAYS be present.
func TestBuildForemanPrompt_BuiltinFallback(t *testing.T) {
	p := PackedProject{Name: "demo", Workdir: "/home/kara/demo"}
	out := buildForemanPrompt(p, "demo-2026-08-27-01-02-03")

	if !strings.Contains(out, "[Scheduler tick: demo-2026-08-27-01-02-03]") {
		t.Error("missing tick-id prefix")
	}
	if !strings.Contains(out, "coding-hermes-map") || !strings.Contains(out, "gitreins task complete") {
		t.Error("built-in prompt body missing")
	}
	if !strings.Contains(out, "Workdir: /home/kara/demo.") {
		t.Error("dynamic workdir footer missing")
	}
	if !strings.Contains(out, "Worker model/provider: .") {
		t.Error("dynamic worker footer missing")
	}
}

func TestBuildForemanPrompt_NamespaceDefault(t *testing.T) {
	p := PackedProject{
		Name:            "demo",
		Workdir:         "/home/kara/demo",
		NamespacePrompt: "CUSTOM NAMESPACE PROMPT: run the duckbrain sync loop.",
	}
	out := buildForemanPrompt(p, "demo-tick")

	if !strings.Contains(out, "CUSTOM NAMESPACE PROMPT") {
		t.Error("namespace default_prompt not used")
	}
	if strings.Contains(out, "coding-hermes-map") {
		t.Error("built-in leaked into a configured namespace prompt")
	}
	if !strings.Contains(out, "Workdir: /home/kara/demo.") {
		t.Error("footer must survive namespace defaults")
	}
}

func TestBuildForemanPrompt_AppendMode(t *testing.T) {
	p := PackedProject{
		Name:            "demo",
		Workdir:         "/home/kara/demo",
		NamespacePrompt: "NS BASE",
		Prompt:          "PROJECT EXTRA: always check the CI matrix.",
		PromptMode:      "append",
	}
	out := buildForemanPrompt(p, "demo-tick")

	if !strings.Contains(out, "NS BASE") || !strings.Contains(out, "PROJECT EXTRA") {
		t.Error("append mode must contain both namespace default and project prompt")
	}
	if !strings.Contains(out, "CI matrix") {
		t.Error("project prompt content missing")
	}
	// Project text must come AFTER the namespace base.
	if strings.Index(out, "NS BASE") > strings.Index(out, "PROJECT EXTRA") {
		t.Error("project prompt should append after namespace default")
	}
}

func TestBuildForemanPrompt_ReplaceMode(t *testing.T) {
	p := PackedProject{
		Name:            "demo",
		Workdir:         "/home/kara/demo",
		NamespacePrompt: "NS BASE (must be replaced)",
		Prompt:          "REPLACEMENT PROMPT ONLY",
		PromptMode:      "replace",
	}
	out := buildForemanPrompt(p, "demo-tick")

	if strings.Contains(out, "NS BASE") {
		t.Error("replace mode must drop the namespace default")
	}
	if !strings.Contains(out, "REPLACEMENT PROMPT ONLY") {
		t.Error("replace mode must use the project prompt")
	}
	if !strings.Contains(out, "Workdir: /home/kara/demo.") {
		t.Error("dynamic footer must survive replace mode")
	}
}

func TestBuildForemanPrompt_WorkerDefaultsInjected(t *testing.T) {
	p := PackedProject{
		Name:           "demo",
		Workdir:        "/home/kara/demo",
		WorkerModel:    "kimi-k3",
		WorkerProvider: "kimi-for-coding",
	}
	out := buildForemanPrompt(p, "demo-tick")

	if !strings.Contains(out, "use model kimi-k3 with provider kimi-for-coding") {
		t.Errorf("worker defaults must be injected, got: %s", out)
	}
}
