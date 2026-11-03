package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/luizeduardocarvalho/fixlab/internal/implement"
	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

// reviewTestsCmd has an agent review a task's hidden tests against its
// refined spec before they grade anyone: tests that would reject a correct
// fix, tests that fail on the base for the wrong reason, gaps and flakiness.
// It reports only; the report is saved as review.md with the transcript.
func reviewTestsCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("review-tests", flag.ExitOnError)
	var c common
	c.register(fs)
	taskPath := fs.String("task", "", "task file (required)")
	redCheck := fs.String("red-check", "", "go test -json output of the tests on the base (default: the latest write-tests red-check.json)")
	rf := registerReadOnly(fs, "prompts/review-tests-v1.md")
	_ = fs.Parse(args)

	if *taskPath == "" {
		fs.Usage()
		return 2
	}
	cfg, err := c.config()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	tk, err := task.Read(*taskPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if strings.TrimSpace(tk.Refined) == "" {
		fmt.Fprintf(os.Stderr, "task %s has no refined.md to review the tests against\n", tk.ID)
		return 2
	}
	var tests strings.Builder
	for _, h := range tk.HiddenTests {
		src, err := os.ReadFile(filepath.Join(tk.Dir, h.From))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		fmt.Fprintf(&tests, "// File: %s\n%s\n", h.To, src)
	}
	logPath := *redCheck
	if logPath == "" {
		if logPath, err = latestRedCheck(cfg.OutDir, tk.ID); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	red, err := summariseGoTest(logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	repoDir, err := cfg.RepoFor(tk.Repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ws := implement.Workspace{Task: tk, Repo: repoDir, Tests: tests.String(), RedCheck: red}
	run, err := runReadOnly(ctx, cfg, rf, "review-tests", ws)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	report := strings.TrimSpace(trimPreamble(run.Answer))
	if err := os.WriteFile(filepath.Join(run.Artifacts, "review.md"), []byte(report+"\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("%s\n\n(review of %s with base results from %s; %s)\n", report, tk.ID, logPath, run.Summary)
	return 0
}

// latestRedCheck returns the red-check.json of the task's latest write-tests
// run.
func latestRedCheck(outDir, id string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(outDir, id, id+"-write-tests-*", "red-check.json"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no write-tests red-check.json for %s under %s; pass -red-check", id, outDir)
	}
	sort.Strings(matches) // names end in a UTC timestamp
	return matches[len(matches)-1], nil
}

// summariseGoTest turns go test -json output into one line per top-level
// test, with the first lines of output for the ones that failed.
func summariseGoTest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	type result struct {
		action  string
		asserts []string // the test's own failure lines (file_test.go:N: ...)
		other   []string // anything else it logged
	}
	results := map[string]*result{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e struct{ Action, Test, Output string }
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Test == "" {
			continue
		}
		name, _, _ := strings.Cut(e.Test, "/")
		r := results[name]
		if r == nil {
			r = &result{}
			results[name] = r
			order = append(order, name)
		}
		switch e.Action {
		case "pass", "fail", "skip":
			if e.Test == name {
				r.action = e.Action
			}
		case "output":
			line := strings.TrimSpace(e.Output)
			switch {
			case line == "" || strings.HasPrefix(line, "=== ") || strings.HasPrefix(line, "--- "):
			case strings.Contains(line, "_test.go:"):
				r.asserts = append(r.asserts, line)
			default:
				r.other = append(r.other, line)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if len(order) == 0 {
		return "", fmt.Errorf("%s has no test results", path)
	}
	var b strings.Builder
	for _, name := range order {
		r := results[name]
		fmt.Fprintf(&b, "%s %s\n", firstNonEmpty(r.action, "unknown"), name)
		if r.action == "fail" {
			lines := append(r.asserts, r.other...)
			for _, line := range lines[:min(len(lines), 4)] {
				fmt.Fprintf(&b, "    %s\n", line)
			}
		}
	}
	return b.String(), nil
}
