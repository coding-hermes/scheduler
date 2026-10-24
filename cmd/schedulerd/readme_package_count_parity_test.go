package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var readmePackageCountRe = regexp.MustCompile(`(?i)\b([0-9]+)\s+packages?\b`)

// gettingStartedSection isolates the README section whose package-count claims
// are checked. It returns false if the heading is missing.
func gettingStartedSection(readme string) (string, bool) {
	lines := strings.Split(readme, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "## Getting Started (5 minutes)" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", false
	}
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			return strings.Join(lines[start:i], "\n"), true
		}
	}
	return strings.Join(lines[start:], "\n"), true
}

// readmePackageCountFindings compares every numeric package-count claim in the
// Getting Started section against the package count computed by `go list ./...`.
// Omitting a count is allowed; an undocumented or stale count is not.
func readmePackageCountFindings(readme string, packageCount int) []string {
	section, ok := gettingStartedSection(readme)
	if !ok {
		return []string{"README.md has no Getting Started section"}
	}

	var findings []string
	for _, match := range readmePackageCountRe.FindAllStringSubmatch(section, -1) {
		documented, err := strconv.Atoi(match[1])
		if err != nil || documented != packageCount {
			findings = append(findings, "Getting Started package-count claim "+match[0]+
				" does not match go list ./... package count "+strconv.Itoa(packageCount))
		}
	}
	return findings
}

func TestReadmePackageCountParity(t *testing.T) {
	root := repoRoot(t)
	readmePath := filepath.Join(root, "README.md")
	readme, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read %s: %v", readmePath, err)
	}

	cmd := exec.Command("go", "list", "./...")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list ./... from %s: %v", root, err)
	}
	packages := strings.Fields(string(output))
	if len(packages) == 0 {
		t.Fatal("go list ./... returned no packages; refusing to accept a vacuous README count check")
	}

	if findings := readmePackageCountFindings(string(readme), len(packages)); len(findings) != 0 {
		for _, finding := range findings {
			t.Error(finding)
		}
	}
}

func TestReadmePackageCountParity_SelfCheck(t *testing.T) {
	const count = 22
	clean := "## Getting Started (5 minutes)\n\n" +
		"`make test` passes (all 22 packages).\n\n## Next section\n"
	if findings := readmePackageCountFindings(clean, count); len(findings) != 0 {
		t.Fatalf("matching count produced findings: %v", findings)
	}
	if findings := readmePackageCountFindings("## Getting Started (5 minutes)\n\nNo package count here.\n## Next section\n", count); len(findings) != 0 {
		t.Fatalf("omitted count produced findings: %v", findings)
	}
	findings := readmePackageCountFindings(strings.Replace(clean, "22 packages", "18 packages", 1), count)
	if len(findings) != 1 {
		t.Fatalf("stale count produced %d findings, want 1: %v", len(findings), findings)
	}
}
