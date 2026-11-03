package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luizeduardocarvalho/fixlab/internal/result"
	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

func TestPromote_BuildsTestsThenFixBranch(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("git", "init", "-q", "-b", "main", repo)
	run("git", "-C", repo, "config", "user.email", "eval@example.com")
	run("git", "-C", repo, "config", "user.name", "eval")
	write(filepath.Join(repo, "handlers", "a.go"), "package handlers\n")
	run("git", "-C", repo, "add", ".")
	run("git", "-C", repo, "commit", "-q", "-m", "base")
	base := run("git", "-C", repo, "rev-parse", "HEAD")

	evalDir := filepath.Join(root, "eval")
	taskDir := filepath.Join(evalDir, "tasks", "issue-13-x")
	write(filepath.Join(taskDir, "testdata", "issue13_test.go"), "package handlers\n\nfunc TestIssue13_X(t *testing.T) {}\n")
	taskPath := filepath.Join(taskDir, "task.yaml")
	write(taskPath, taskYAML("issue-13-x", "feature", "inventory", "api", base, "Fix x\n\nbody\n", "handlers/issue13_test.go", map[string]int{"contract": 1}))
	tk, err := task.Read(taskPath)
	if err != nil {
		t.Fatal(err)
	}

	resDir := filepath.Join(evalDir, "results", "issue-13-x")
	write(filepath.Join(resDir, "r1", "candidate.diff"), "diff --git a/handlers/a.go b/handlers/a.go\n--- a/handlers/a.go\n+++ b/handlers/a.go\n@@ -1 +1,3 @@\n package handlers\n+\n+var fixed = true\n")
	res := result.Result{TaskID: tk.ID, RunID: "r1", Pass: true, TaskSHA256: tk.SHA256(), Strategy: "refine-then-implement"}
	raw, _ := json.Marshal(res)
	write(filepath.Join(resDir, "r1.json"), string(raw))

	code := promoteCmd(context.Background(), []string{"-workspace", evalDir, "-api-repo", repo,
		"-task", taskPath, "-result", filepath.Join(resDir, "r1.json"), "-branch", "fix/13-x", "-subject", "fix: make x right"})
	if code != 0 {
		t.Fatalf("promote exited %d", code)
	}
	log := run("git", "-C", repo, "log", "--format=%s", base+"..fix/13-x")
	if log != "fix: make x right\ntest: add acceptance tests for #13" {
		t.Errorf("log = %q", log)
	}
	if body := run("git", "-C", repo, "log", "--format=%b", base+"..fix/13-x"); strings.Count(body, agentTrailer) != 2 {
		t.Errorf("want the agent trailer on both commits, got:\n%s", body)
	}
	if got := run("git", "-C", repo, "show", "fix/13-x~1:handlers/issue13_test.go"); !strings.Contains(got, "TestIssue13_X") {
		t.Errorf("tests commit lacks the hidden test: %q", got)
	}
	if got := run("git", "-C", repo, "show", "fix/13-x:handlers/a.go"); !strings.Contains(got, "fixed = true") {
		t.Errorf("fix commit lacks the attempt's change: %q", got)
	}
	tk, err = task.Read(taskPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := run("git", "-C", repo, "rev-parse", "fix/13-x"); tk.ReferenceCommit != want {
		t.Errorf("reference_commit = %q, want %q", tk.ReferenceCommit, want)
	}
	if code := promoteCmd(context.Background(), []string{"-workspace", evalDir, "-api-repo", repo,
		"-task", taskPath, "-result", filepath.Join(resDir, "r1.json"), "-branch", "fix/13-y"}); code == 0 {
		t.Error("a second promote succeeded; want a refusal")
	}
}
