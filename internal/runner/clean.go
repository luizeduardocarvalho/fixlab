package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Clean removes everything a crashed or killed harness may have left behind:
// compose projects named fixlab-* (grading stacks and envs, of every
// workspace on the machine), their built images, env state, and git
// worktrees under cfg.WorkDir. Normal runs clean up after themselves.
func Clean(ctx context.Context, cfg Config) error {
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "compose", "ls", "-a", "--format", "json").Output()
	if err != nil {
		return err
	}
	var projects []struct{ Name string }
	if err := json.Unmarshal(out, &projects); err != nil {
		return err
	}
	// Run from an empty dir so compose resolves the project by name only.
	empty, err := os.MkdirTemp("", "fixlab-clean")
	if err != nil {
		return err
	}
	defer os.RemoveAll(empty)
	for _, p := range projects {
		if !strings.HasPrefix(p.Name, ProjectPrefix) {
			continue
		}
		logf("removing compose project %s", p.Name)
		cmd := exec.CommandContext(ctx, "docker", "compose", "-p", p.Name, "down", "-v", "--remove-orphans", "--timeout", "5")
		cmd.Dir = empty
		if out, err := cmd.CombinedOutput(); err != nil {
			logf("compose down %s: %v: %s", p.Name, err, out)
		}
	}

	images, err := exec.CommandContext(ctx, "docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(strings.NewReader(string(images)))
	for sc.Scan() {
		if ref := sc.Text(); strings.HasPrefix(ref, ProjectPrefix) {
			logf("removing image %s", ref)
			_ = exec.CommandContext(ctx, "docker", "image", "rm", "-f", ref).Run()
		}
	}

	workDir, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return err
	}
	for _, repo := range []string{cfg.APIRepo, cfg.WebRepo} {
		if repo == "" {
			continue
		}
		out, err := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "list", "--porcelain").Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			path, ok := strings.CutPrefix(line, "worktree ")
			if !ok || !strings.HasPrefix(path, workDir+string(filepath.Separator)) {
				continue
			}
			logf("removing worktree %s", path)
			_ = exec.CommandContext(ctx, "git", "-C", repo, "worktree", "remove", "--force", path).Run()
		}
		_ = exec.CommandContext(ctx, "git", "-C", repo, "worktree", "prune").Run()
	}
	if root, err := EnvRoot(); err == nil {
		_ = os.RemoveAll(root)
	}
	return os.RemoveAll(workDir)
}
