package main

import (
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

func TestRetentionReportDoesNotClaimMetricsWereChecked(t *testing.T) {
	source := t.TempDir() + "/source.db"
	db, err := database.InitDB(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	output := t.TempDir() + "/copy.db"
	var stdout, stderr strings.Builder
	if code := run([]string{"--db", source, "--output", output, "--retention", "0"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "metrics_shapes=unchecked") {
		t.Fatalf("report omitted unchecked metrics shape: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "metrics_shapes=unchanged") {
		t.Fatalf("report made an unchecked metrics-shape claim: %q", stdout.String())
	}
}
