package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/luizeduardocarvalho/fixlab/internal/implement"
	"github.com/luizeduardocarvalho/fixlab/internal/result"
	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

// promoteCmd turns a passing attempt into the task's reference solution: a
// branch off base_commit with the hidden tests in one commit and the
// attempt's change in the next, recorded as reference_commit in task.yaml.
// It creates a local branch only; pushing it is left to the caller.
func promoteCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("promote", flag.ExitOnError)
	var c common
	c.register(fs)
	taskPath := fs.String("task", "", "task file (required)")
	resPath := fs.String("result", "", "result JSON of a passing attempt, e.g. results/<task>/<run_id>.json (required)")
	branch := fs.String("branch", "", "new branch name, e.g. fix/13-low-stock (required)")
	subject := fs.String("subject", "", `fix commit subject in the repo's style, e.g. "fix: define low stock once" (default: "fix: <task id>")`)
	_ = fs.Parse(args)

	if *taskPath == "" || *resPath == "" || *branch == "" {
		fs.Usage()
		return 2
	}
	cfg, err := c.config()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	t, err := task.Read(*taskPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	raw, err := os.ReadFile(*resPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	var res result.Result
	if err := json.Unmarshal(raw, &res); err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", *resPath, err)
		return 2
	}
	switch {
	case res.TaskID != t.ID:
		err = fmt.Errorf("the result is for task %s, not %s", res.TaskID, t.ID)
	case !res.Pass:
		err = fmt.Errorf("attempt %s did not pass", res.RunID)
	case res.TaskSHA256 != t.SHA256():
		err = fmt.Errorf("attempt %s was graded by an older version of the task; regrade it first", res.RunID)
	case t.ReferenceCommit != "":
		err = fmt.Errorf("task %s already has reference_commit %s", t.ID, t.ReferenceCommit)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	diff, err := os.ReadFile(filepath.Join(strings.TrimSuffix(*resPath, ".json"), "candidate.diff"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	repo, err := cfg.RepoFor(t.Repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	wt, err := os.MkdirTemp(cfg.WorkDir, "promote-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		_, _ = gitOut(context.Background(), repo, "worktree", "remove", "--force", wt)
		os.RemoveAll(wt)
	}()
	if _, err := gitOut(ctx, repo, "worktree", "add", "-b", *branch, wt, t.BaseCommit); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ref, err := promoteCommits(ctx, wt, t, res, diff, firstNonEmpty(*subject, "fix: "+t.ID))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		_, _ = gitOut(context.Background(), repo, "worktree", "remove", "--force", wt)
		_, _ = gitOut(context.Background(), repo, "branch", "-D", *branch)
		return 1
	}
	if err := setReferenceCommit(*taskPath, ref, *branch, res.RunID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("branch %s at %s: hidden tests, then attempt %s\nset reference_commit in %s\n", *branch, ref[:7], res.RunID, *taskPath)
	return 0
}

func promoteCommits(ctx context.Context, wt string, t task.Task, res result.Result, diff []byte, subject string) (string, error) {
	for _, h := range t.HiddenTests {
		src, err := os.ReadFile(filepath.Join(t.Dir, h.From))
		if err != nil {
			return "", err
		}
		dst := filepath.Join(wt, h.To)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(dst, src, 0o644); err != nil {
			return "", err
		}
		if _, err := gitOut(ctx, wt, "add", h.To); err != nil {
			return "", err
		}
	}
	title := strings.SplitN(strings.TrimSpace(t.Description), "\n", 2)[0]
	testSubject := "test: add acceptance tests for " + t.ID
	if prefix, _ := implement.TestPrefixes(t.ID); prefix != "" {
		testSubject = "test: add acceptance tests for #" + strings.TrimSuffix(strings.TrimPrefix(prefix, "TestIssue"), "_")
	}
	if _, err := gitOut(ctx, wt, "commit", "-q", "-m", testSubject, "-m", title, "--trailer", agentTrailer); err != nil {
		return "", err
	}
	patch := filepath.Join(wt, ".git-promote.diff")
	if err := os.WriteFile(patch, diff, 0o644); err != nil {
		return "", err
	}
	defer os.Remove(patch)
	if _, err := gitOut(ctx, wt, "apply", "--index", "--binary", "--whitespace=nowarn", patch); err != nil {
		return "", fmt.Errorf("apply the attempt's diff on top of the tests: %w", err)
	}
	msg := fmt.Sprintf("Promoted from eval attempt %s (strategy %s, model %s).", res.RunID, firstNonEmpty(res.Strategy, "none"), firstNonEmpty(res.Model, "default"))
	if _, err := gitOut(ctx, wt, "commit", "-q", "-m", subject, "-m", msg, "--trailer", agentTrailer); err != nil {
		return "", err
	}
	return gitOut(ctx, wt, "rev-parse", "HEAD")
}

// agentTrailer credits the agent that wrote both commits' content.
const agentTrailer = "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"

var referenceLine = regexp.MustCompile(`(?m)^#? ?reference_commit:.*$`)

// setReferenceCommit replaces the reference_commit line (or the placeholder
// comment fixlab write-tests leaves) in the task file, keeping the rest.
func setReferenceCommit(path, ref, branch, runID string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("# Known-good solution: branch %s, tests then attempt %s.\nreference_commit: %s", branch, runID, ref)
	if !referenceLine.Match(src) {
		return fmt.Errorf("%s has no reference_commit line to set", path)
	}
	return os.WriteFile(path, referenceLine.ReplaceAll(src, []byte(line)), 0o644)
}
