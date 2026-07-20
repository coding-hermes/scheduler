package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestIsCodingHermesJob(t *testing.T) {
	tests := []struct {
		name string
		job  CronJob
		want bool
	}{
		{"skills contain coding-hermes", CronJob{Skills: []string{"coding-hermes"}}, true},
		{"skills contain coding-hermes-worker", CronJob{Skills: []string{"coding-hermes-worker"}}, true},
		{"name contains coding-hermes lower", CronJob{Name: "alpha coding-hermes"}, true},
		{"name contains coding-hermes mixed", CronJob{Name: "Alpha Coding-Hermes"}, true},
		{"name contains foreman lower", CronJob{Name: "alpha foreman"}, true},
		{"name contains foreman mixed", CronJob{Name: "Alpha Foreman"}, true},
		{"no match skills", CronJob{Skills: []string{"foo"}}, false},
		{"no match name", CronJob{Name: "alpha-beta"}, false},
		{"empty job", CronJob{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isCodingHermesJob(tt.job)
			if got != tt.want {
				t.Errorf("isCodingHermesJob() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractWorkdir(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   string
	}{
		{"workdir colon", "Run in workdir: /tmp/foo", "/tmp/foo"},
		{"workdir capital", "Workdir: /tmp/bar", "/tmp/bar"},
		{"workdir trailing punctuation", "workdir: /tmp/foo,", "/tmp/foo"},
		{"workdir trailing semicolon", "workdir: /tmp/foo;", "/tmp/foo"},
		{"workdir trailing colon", "workdir: /tmp/foo:", "/tmp/foo"},
		{"workdir multiple spaces", "workdir:    /tmp/foo", "/tmp/foo"},
		{"no workdir", "some other prompt", ""},
		{"empty prompt", "", ""},
		{"workdir regex stops at whitespace", "workdir: /tmp/path with spaces", "/tmp/path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractWorkdir(CronJob{Prompt: tt.prompt})
			if got != tt.want {
				t.Errorf("extractWorkdir() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProjectName(t *testing.T) {
	tests := []struct {
		name     string
		job      CronJob
		expected string
	}{
		{"workdir basename", CronJob{Prompt: "workdir: /home/user/projects/foo-bar"}, "foo-bar"},
		{"workdir basename with trailing slash", CronJob{Prompt: "workdir: /home/user/projects/foo-bar/"}, "foo-bar"},
		{"fallback name strip coding-hermes", CronJob{Name: "my-project coding-hermes"}, "my-project"},
		{"fallback name strip foreman", CronJob{Name: "my-project coding-hermes-foreman"}, "my-project"},
		{"fallback name spaces to dashes", CronJob{Name: "My Cool Project coding-hermes"}, "my-cool-project"},
		{"fallback unknown empty name", CronJob{Name: "", ID: "12345678-1234-1234-1234-123456789abc"}, "unknown-12345678"},
		{"fallback unknown coding-hermes name", CronJob{Name: "coding-hermes", ID: "12345678-1234-1234-1234-123456789abc"}, "unknown-12345678"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := projectName(tt.job)
			if got != tt.expected {
				t.Errorf("projectName() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestLoadJobs(t *testing.T) {
	t.Run("valid json", func(t *testing.T) {
		tmp := writeTempFile(t, `{"jobs": [{"id":"1","name":"a"}]}`)
		jobs, err := loadJobs(tmp)
		if err != nil {
			t.Fatalf("loadJobs() error = %v", err)
		}
		if len(jobs) != 1 || jobs[0].ID != "1" || jobs[0].Name != "a" {
			t.Fatalf("unexpected jobs: %+v", jobs)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := loadJobs("/nonexistent/path/jobs.json")
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		tmp := writeTempFile(t, `{invalid json}`)
		_, err := loadJobs(tmp)
		if err == nil {
			t.Fatal("expected error for invalid json")
		}
	})

	t.Run("empty jobs array", func(t *testing.T) {
		tmp := writeTempFile(t, `{"jobs": []}`)
		jobs, err := loadJobs(tmp)
		if err != nil {
			t.Fatalf("loadJobs() error = %v", err)
		}
		if len(jobs) != 0 {
			t.Fatalf("expected 0 jobs, got %d", len(jobs))
		}
	})
}

func TestCronJobUnmarshal(t *testing.T) {
	data := `{
		"id": "job-1",
		"name": "alpha coding-hermes",
		"skills": ["coding-hermes-worker"],
		"model": "gpt-4",
		"provider": "openai",
		"enabled": true,
		"schedule": {
			"kind": "cron",
			"expr": "0 * * * *",
			"minutes": 0,
			"display": "hourly"
		},
		"prompt": "workdir: /tmp/alpha"
	}`
	var j CronJob
	if err := json.Unmarshal([]byte(data), &j); err != nil {
		t.Fatalf("Unmarshal error = %v", err)
	}
	if j.ID != "job-1" {
		t.Errorf("ID = %q, want %q", j.ID, "job-1")
	}
	if j.Name != "alpha coding-hermes" {
		t.Errorf("Name = %q, want %q", j.Name, "alpha coding-hermes")
	}
	if len(j.Skills) != 1 || j.Skills[0] != "coding-hermes-worker" {
		t.Errorf("Skills = %v, want [coding-hermes-worker]", j.Skills)
	}
	if j.Model != "gpt-4" {
		t.Errorf("Model = %q, want %q", j.Model, "gpt-4")
	}
	if j.Provider != "openai" {
		t.Errorf("Provider = %q, want %q", j.Provider, "openai")
	}
	if !j.Enabled {
		t.Error("Enabled = false, want true")
	}
	if j.Schedule.Kind != "cron" {
		t.Errorf("Schedule.Kind = %q, want %q", j.Schedule.Kind, "cron")
	}
	if j.Schedule.Expr != "0 * * * *" {
		t.Errorf("Schedule.Expr = %q, want %q", j.Schedule.Expr, "0 * * * *")
	}
	if j.Schedule.Minutes != 0 {
		t.Errorf("Schedule.Minutes = %d, want %d", j.Schedule.Minutes, 0)
	}
	if j.Schedule.Display != "hourly" {
		t.Errorf("Schedule.Display = %q, want %q", j.Schedule.Display, "hourly")
	}
	if j.Prompt != "workdir: /tmp/alpha" {
		t.Errorf("Prompt = %q, want %q", j.Prompt, "workdir: /tmp/alpha")
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	tmpDir := t.TempDir()
	p := filepath.Join(tmpDir, "jobs.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return p
}

func captureStdout(f func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	f()
	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	os.Stdout = old
	return buf.String()
}
