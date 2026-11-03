package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

func TestTaskYAML_Loads(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "testdata"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "testdata", "issue13_test.go"), []byte("package handlers\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	issue := "Title line\n\n## Problem\n- `a: b` and {{braces}}\n"
	src := taskYAML("issue-13-x", "feature", "inventory", "api", strings.Repeat("a", 40), issue, "handlers/issue13_test.go", map[string]int{"contract": 2, "control": 1})
	if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := task.Read(filepath.Join(dir, "task.yaml"))
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	if got.Description != issue {
		t.Errorf("description = %q, want %q", got.Description, issue)
	}
	if len(got.HiddenTests) != 1 || got.HiddenTests[0].To != "handlers/issue13_test.go" || got.HiddenTests[0].From != "testdata/issue13_test.go" {
		t.Errorf("hidden tests = %+v", got.HiddenTests)
	}
}

func TestTestKind(t *testing.T) {
	for name, want := range map[string]string{
		"TestIssue13_ConcurrentUse":       "concurrency",
		"TestIssue13_OtherLabMaterial":    "security",
		"TestIssue13_ControlExpiringSoon": "control",
		"TestIssue13_MaterialIsLowStock":  "contract",
		"TestIssue13_ConcurrentOtherLab":  "concurrency",
	} {
		if got := testKind("TestIssue13_", name); got != want {
			t.Errorf("testKind(%s) = %s, want %s", name, got, want)
		}
	}
}

func TestWriteTestKinds_OnlyKindsWithTests(t *testing.T) {
	got := writeTestKinds("TestIssue13_", map[string]int{"contract": 3, "security": 1})
	if strings.Contains(got, "concurrency") || strings.Contains(got, "kind: control") {
		t.Errorf("kinds without tests listed:\n%s", got)
	}
	if !strings.Contains(got, `tests: ["TestIssue13_*OtherLab*"]`) || !strings.HasSuffix(got, "tests: [\"TestIssue13_*\"]\n") {
		t.Errorf("want security, then contract as the catch-all last:\n%s", got)
	}
}
