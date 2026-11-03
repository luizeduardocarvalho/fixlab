package implement

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegrade_ReplaysDiffAndKeepsAgentFields(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	write(t, filepath.Join(repo, "app.go"), "broken\n")
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "base")
	worktree := filepath.Join(root, "cand")
	gitRun(t, repo, "worktree", "add", "-q", "--detach", worktree, "HEAD")

	attempt := filepath.Join(root, "attempt")
	write(t, filepath.Join(attempt, "result.json"), `{"run_id":"t1-r1-abc","task_id":"t1","implementer":"claude:claude-opus-5-5",
		"implementer_kind":"agent","model":"claude-opus-5-5","prompt_variant":"issue-v1","claude_md_variant":"repo",
		"agent_outcome":"success","agent_cost_usd":1.25,"agent_turns":30,"agent_denied_path_refs":0}`)
	write(t, filepath.Join(attempt, "candidate.diff"), "diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n@@ -1 +1 @@\n-broken\n+fixed\n")
	write(t, filepath.Join(attempt, "transcript.jsonl"), "{}\n")

	g := Regrade{AttemptDir: attempt}
	if g.Name() != "claude:claude-opus-5-5" {
		t.Fatalf("name: %q", g.Name())
	}
	if run, task, err := g.Source(); err != nil || run != "t1-r1-abc" || task != "t1" {
		t.Fatalf("source: %q %q %v", run, task, err)
	}
	art := filepath.Join(root, "art")
	info, err := g.Implement(context.Background(), Workspace{Worktree: worktree, Artifacts: art})
	if err != nil {
		t.Fatal(err)
	}
	if info.Kind != "agent" || info.Model != "claude-opus-5-5" || info.CostUSD != 1.25 || info.Turns != 30 || info.PromptVariant != "issue-v1" {
		t.Fatalf("info not carried over: %+v", info)
	}
	if b, _ := os.ReadFile(filepath.Join(worktree, "app.go")); strings.TrimSpace(string(b)) != "fixed" {
		t.Fatalf("diff not applied: %q", b)
	}
	if _, err := os.Stat(filepath.Join(art, "transcript.jsonl")); err != nil {
		t.Fatal("original transcript not kept with the regrade")
	}
}
