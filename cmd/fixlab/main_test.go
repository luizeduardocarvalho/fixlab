package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/luizeduardocarvalho/fixlab/internal/runner"
)

func TestSnapshotStrip_IsTheWorkspacePathInItsRepo(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	ws := filepath.Join(repo, "eval")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := snapshotStrip(runner.Config{Workspace: ws}); len(got) != 1 || got[0] != "eval" {
		t.Fatalf("workspace inside the repo: got %v", got)
	}
	// At the repo root, or outside any repo, there is nothing to strip:
	// stripping "" would empty the snapshot.
	if got := snapshotStrip(runner.Config{Workspace: repo}); got != nil {
		t.Fatalf("workspace at the repo root: got %v", got)
	}
	if got := snapshotStrip(runner.Config{Workspace: t.TempDir()}); got != nil {
		t.Fatalf("workspace outside a repo: got %v", got)
	}
}

func TestResolveHistory_FlagThenEnv(t *testing.T) {
	t.Setenv(historyEnv, "")
	if got := resolveHistory(""); got != "" {
		t.Fatalf("no flag, no env: got %q", got)
	}
	t.Setenv(historyEnv, "/from/env")
	if got := resolveHistory(""); got != "/from/env" {
		t.Fatalf("env: got %q", got)
	}
	if got := resolveHistory("/from/flag"); got != "/from/flag" {
		t.Fatalf("flag wins: got %q", got)
	}
}

func TestDeniedDirs_SkipsAMissingWebRepo(t *testing.T) {
	got := deniedDirs(runner.Config{APIRepo: "/p/api", Workspace: "/p/api/eval"}, "/p/history")
	want := []string{"/p", "/p/api/eval", "/p/history"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
