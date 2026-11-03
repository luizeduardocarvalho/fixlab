package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRead_ExampleTask(t *testing.T) {
	tk, err := Read("../../testdata/example/tasks/issue-1-scope-lookups/task.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Repo != RepoAPI || tk.TaskType != "bug_fix" || tk.Load.VUs != 10 || len(tk.HiddenTests) != 1 {
		t.Fatalf("unexpected task: %+v", tk)
	}
}

func TestRead_RejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.yaml")
	if err := os.WriteFile(path, []byte("id: Bad ID\ntask_type: chore\nhidden_tests:\n  - from: missing.go\n    to: ../escape.go\nunknown_field: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil || !strings.Contains(err.Error(), "unknown_field") {
		t.Fatalf("want unknown-field error, got %v", err)
	}
	if err := os.WriteFile(path, []byte("id: Bad ID\ntask_type: chore\nhidden_tests:\n  - from: missing.go\n    to: ../escape.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Read(path)
	for _, want := range []string{"id must match", "description is required", "base_commit is required", "task_type must be", "area is required", "missing.go", "inside the repo"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want error containing %q, got %v", want, err)
		}
	}
}

func TestHiddenGoTests(t *testing.T) {
	dir := t.TempDir()
	src := "package x\n\nfunc TestOne(t *testing.T) {}\nfunc helper(t *testing.T) {}\nfunc TestTwo(tt *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	tk := Task{Dir: dir, HiddenTests: []HiddenTest{{From: "x_test.go", To: "pkg/x_test.go"}}}
	names, err := tk.HiddenGoTests()
	if err != nil || len(names) != 2 || names[0] != "TestOne" || names[1] != "TestTwo" {
		t.Fatalf("got %v, %v", names, err)
	}
}

func TestSHA256_IsStable(t *testing.T) {
	tk, err := Read("../../testdata/example/tasks/issue-1-scope-lookups/task.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Histories key attempts by this hash: a change to how it is computed
	// splits every project's history, so it must be deliberate.
	if got := tk.SHA256(); !strings.HasPrefix(got, "41d9260d") {
		t.Fatalf("task version changed: %s", got)
	}
}

func TestGradingSHA256_IgnoresReferenceCommitButNotTests(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("x_test.go", "package x\n")
	write("task.yaml", "reference_commit: aaa\n")
	tk := Task{Dir: dir, BaseCommit: "base1", HiddenTests: []HiddenTest{{From: "x_test.go", To: "pkg/x_test.go"}}}
	grading, version := tk.GradingSHA256(), tk.SHA256()

	write("task.yaml", "reference_commit: bbb\n")
	if tk.SHA256() == version {
		t.Fatal("SHA256 should change with task.yaml")
	}
	if tk.GradingSHA256() != grading {
		t.Fatal("moving reference_commit changed GradingSHA256")
	}

	write("x_test.go", "package x\n\nfunc TestNew(t *testing.T) {}\n")
	if tk.GradingSHA256() == grading {
		t.Fatal("changing a hidden test did not change GradingSHA256")
	}
	changedTests := tk.GradingSHA256()
	tk.BaseCommit = "base2"
	if tk.GradingSHA256() == changedTests {
		t.Fatal("changing the base commit did not change GradingSHA256")
	}
}
