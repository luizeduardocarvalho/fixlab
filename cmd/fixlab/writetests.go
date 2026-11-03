package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/luizeduardocarvalho/fixlab/internal/implement"
	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

// writeTestsCmd has an agent write a task's hidden tests from its refined
// spec, on the unfixed base, then checks them there: they must compile,
// contract and concurrency tests must fail, and control tests must pass. It
// writes testdata/, task.yaml (without a reference yet) and test-kinds.yaml.
func writeTestsCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("write-tests", flag.ExitOnError)
	var c common
	c.register(fs)
	dir := fs.String("task-dir", "", "task directory holding issue-as-filed.md and refined.md (required)")
	base := fs.String("base", "", "commit the tests are written against (required)")
	testFile := fs.String("test-file", "", "repo-relative path of the new test file, e.g. handlers/issue13_test.go (required)")
	taskType := fs.String("type", "feature", "task_type for task.yaml")
	area := fs.String("area", "", "area for task.yaml, e.g. inventory (required)")
	repo := fs.String("repo", task.RepoAPI, "api (web is not supported yet)")
	model := fs.String("model", "", "agent model (default: the CLI's default)")
	effort := fs.String("effort", "", "agent effort level")
	prompt := fs.String("prompt", "prompts/write-tests-v1.md", "prompt template, relative to -workspace")
	claudeMD := fs.String("claude-md", "", `CLAUDE.md the agent sees: "" = the repo's, "none", or a file`)
	agentCLI := fs.String("agent-cli", firstNonEmpty(os.Getenv("CLAUDE_BIN"), "claude"), "claude binary (env CLAUDE_BIN)")
	agentConfig := fs.String("agent-config-dir", os.Getenv("CLAUDE_CONFIG_DIR"), "CLAUDE_CONFIG_DIR for the agent's account")
	budget := fs.Float64("agent-budget-usd", 10, "session limit at API list price (0 = none)")
	force := fs.Bool("force", false, "overwrite an existing task.yaml and test file")
	_ = fs.Parse(args)

	if *dir == "" || *base == "" || *testFile == "" || *area == "" {
		fs.Usage()
		return 2
	}
	if *repo != task.RepoAPI {
		fmt.Fprintln(os.Stderr, "write-tests supports only -repo api")
		return 2
	}
	cfg, err := c.config()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	taskDir, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	id := filepath.Base(taskDir)
	issue, err := os.ReadFile(filepath.Join(taskDir, "issue-as-filed.md"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	refined, err := os.ReadFile(filepath.Join(taskDir, "refined.md"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "write-tests needs the refined spec:", err)
		return 2
	}
	taskPath := filepath.Join(taskDir, "task.yaml")
	testdata := filepath.Join(taskDir, "testdata", filepath.Base(*testFile))
	if !*force {
		for _, p := range []string{taskPath, testdata} {
			if _, err := os.Stat(p); err == nil {
				fmt.Fprintf(os.Stderr, "%s exists; pass -force to overwrite it\n", p)
				return 2
			}
		}
	}
	baseSHA, err := gitOut(ctx, cfg.APIRepo, "rev-parse", "--verify", *base+"^{commit}")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	promptPath := *prompt
	if !filepath.IsAbs(promptPath) {
		promptPath = filepath.Join(cfg.Workspace, promptPath)
	}
	mdPath := *claudeMD
	if mdPath != "" && mdPath != "none" {
		if mdPath, err = filepath.Abs(mdPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}

	runID := fmt.Sprintf("%s-write-tests-%s", id, time.Now().UTC().Format("20060102T150405Z"))
	artifacts := filepath.Join(cfg.OutDir, id, runID)
	p, cleanup, err := taskProfile(ctx, cfg, task.Task{ID: id, Repo: *repo, BaseCommit: baseSHA})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer cleanup()
	agent := implement.Agent{
		CLI: *agentCLI, ConfigDir: *agentConfig, Model: *model, Effort: *effort,
		PromptPath: promptPath, ClaudeMD: mdPath, BudgetUSD: *budget, Timeout: 60 * time.Minute,
		ProjectTools: p.AgentTools(),
		DeniedPaths:  deniedDirs(cfg, resolveHistory("")),
		Strip:        snapshotStrip(cfg),
	}
	ws := implement.Workspace{
		Task: task.Task{
			ID: id, Description: string(issue), BaseCommit: baseSHA, Repo: *repo,
			Dir: taskDir, Refined: string(refined),
		},
		Repo:      cfg.APIRepo,
		Artifacts: artifacts,
		TestFile:  *testFile,
	}
	src, info, err := agent.WriteTests(ctx, ws)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	tok := info.InputTokens + info.OutputTokens + info.CacheReadTokens + info.CacheWriteTokens
	fmt.Printf("agent wrote %s (%d turns, %.2fM tokens, %d denied-path refs; transcript in %s)\n",
		*testFile, info.Turns, float64(tok)/1e6, info.DeniedPathRefs, artifacts)

	if err := os.MkdirAll(filepath.Dir(testdata), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.WriteFile(testdata, src, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	prefix, helper := implement.TestPrefixes(id)

	results, err := hiddenCheck(ctx, cfg.APIRepo, cfg.WorkDir, baseSHA, nil, map[string][]byte{*testFile: src}, prefix, filepath.Join(artifacts, "red-check.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "red check:", err)
		return 1
	}
	var problems []string
	counts := map[string]int{}
	names := make([]string, 0, len(results))
	for name := range results {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Printf("\non base %s:\n", baseSHA[:7])
	for _, name := range names {
		kind, action := testKind(prefix, name), results[name]
		counts[kind]++
		fmt.Printf("  %-6s %-11s %s\n", action, kind, name)
		switch {
		case action == "skip":
			problems = append(problems, name+" skips on base")
		case (kind == "contract" || kind == "concurrency") && action != "fail":
			problems = append(problems, name+" passes on base, so it doesn't test the change")
		case kind == "control" && action != "pass":
			problems = append(problems, name+" fails on base, but a control test must pass there")
		}
	}
	if len(results) == 0 {
		problems = append(problems, "no "+prefix+" tests ran")
	} else if counts["contract"]+counts["concurrency"] == 0 {
		problems = append(problems, "no contract or concurrency tests: nothing fails on base, so the task can't tell a fix from no change")
	}

	if err := os.WriteFile(filepath.Join(taskDir, "test-kinds.yaml"), []byte(writeTestKinds(prefix, counts)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.WriteFile(taskPath, []byte(taskYAML(id, *taskType, *area, *repo, baseSHA, string(issue), *testFile, counts)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("\nwrote %s, %s and %s (helper prefix %s)\n", testdata, taskPath, filepath.Join(taskDir, "test-kinds.yaml"), helper)
	if len(problems) > 0 {
		fmt.Println("\nthe tests need a look:")
		for _, p := range problems {
			fmt.Println("  -", p)
		}
		return 1
	}
	return 0
}

// hiddenCheck applies diff (if any) to a detached worktree of base, adds the
// test files (repo-relative path to content), and returns each top-level
// prefix test's final action. It errors if the code doesn't vet.
func hiddenCheck(ctx context.Context, repo, workDir, base string, diff []byte, tests map[string][]byte, prefix, logPath string) (map[string]string, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	wt, err := os.MkdirTemp(workDir, "check-")
	if err != nil {
		return nil, err
	}
	if _, err := gitOut(ctx, repo, "worktree", "add", "--detach", wt, base); err != nil {
		os.RemoveAll(wt)
		return nil, err
	}
	defer func() {
		_, _ = gitOut(context.Background(), repo, "worktree", "remove", "--force", wt)
		os.RemoveAll(wt)
	}()
	if len(diff) > 0 {
		patch := filepath.Join(workDir, filepath.Base(wt)+".diff")
		if err := os.WriteFile(patch, diff, 0o644); err != nil {
			return nil, err
		}
		defer os.Remove(patch)
		if _, err := gitOut(ctx, wt, "apply", "--binary", "--whitespace=nowarn", patch); err != nil {
			return nil, err
		}
	}
	pkgs := map[string]bool{}
	for path, src := range tests {
		dst := filepath.Join(wt, path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, src, 0o644); err != nil {
			return nil, err
		}
		pkgs["./"+filepath.ToSlash(filepath.Dir(path))+"/"] = true
	}
	var out []byte
	env := append(implement.SafeEnv(), "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local")
	for pkg := range pkgs {
		vet := exec.CommandContext(ctx, "go", "vet", pkg)
		vet.Dir, vet.Env = wt, env
		if o, err := vet.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("go vet %s: %w\n%s", pkg, err, o)
		}
		test := exec.CommandContext(ctx, "go", "test", "-json", "-count=1", "-timeout=15m", "-run", "^"+prefix, pkg)
		test.Dir, test.Env = wt, env
		o, _ := test.Output() // failing tests are expected here; read the events
		out = append(out, o...)
	}
	if err := os.WriteFile(logPath, out, 0o644); err != nil {
		return nil, err
	}
	results := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	buildFailed := false
	for sc.Scan() {
		var ev struct{ Action, Test string }
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Action == "build-fail" {
			buildFailed = true
		}
		if ev.Test == "" || strings.Contains(ev.Test, "/") {
			continue
		}
		if ev.Action == "pass" || ev.Action == "fail" || ev.Action == "skip" {
			results[ev.Test] = ev.Action
		}
	}
	if buildFailed {
		return nil, fmt.Errorf("the tests don't build; see %s", logPath)
	}
	return results, nil
}

// testKind matches writeTestKinds: the first pattern a name matches wins.
func testKind(prefix, name string) string {
	rest := strings.TrimPrefix(name, prefix)
	switch {
	case strings.Contains(rest, "Concurrent"):
		return "concurrency"
	case strings.Contains(rest, "OtherLab"):
		return "security"
	case strings.Contains(rest, "Control"):
		return "control"
	default:
		return "contract"
	}
}

// writeTestKinds lists the kinds that have tests, in testKind's order. The
// catch-all contract pattern comes last, as testKind's default does.
func writeTestKinds(prefix string, counts map[string]int) string {
	var b strings.Builder
	b.WriteString(`# Groups the hidden tests in the report (fixlab report). Not part of the
# grader: editing this file does not change the task version. Patterns are
# shell globs on top-level test names; a test takes the first kind it matches.
# Written by fixlab write-tests from the naming rules in prompts/write-tests-v1.md.
`)
	for _, k := range []struct{ kind, label, pattern string }{
		{"concurrency", "Correct under concurrent requests", "*Concurrent*"},
		{"security", "Other labs' data unreachable", "*OtherLab*"},
		{"control", "Existing behaviour unchanged", "*Control*"},
		{"contract", "Status codes and data effects", ""},
	} {
		if counts[k.kind] > 0 {
			fmt.Fprintf(&b, "- kind: %s\n  label: %s\n  tests: [\"%s%s\"]\n", k.kind, k.label, prefix, firstNonEmpty(k.pattern, "*"))
		}
	}
	return b.String()
}

func taskYAML(id, taskType, area, repo, base, issue, testFile string, counts map[string]int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "id: %s\ntask_type: %s\narea: %s\nrepo: %s\n\nbase_commit: %s\n\n", id, taskType, area, repo, base)
	b.WriteString("# reference_commit: set it once a passing run is promoted to a fix branch.\n\n")
	b.WriteString("# Prompt for an implementing agent: the issue as filed, verbatim. The\n# refined spec is refined.md ({{.Refined}} in the prompt).\ndescription: |\n")
	for _, line := range strings.Split(strings.TrimRight(issue, "\n"), "\n") {
		if line == "" {
			b.WriteString("\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	fmt.Fprintf(&b, "\n# Grader written by fixlab write-tests (%s) from refined.md.\n", time.Now().UTC().Format("2006-01-02"))
	fmt.Fprintf(&b, "# %d top-level tests: concurrency (%d), contract (%d), security (%d), control (%d);\n# see test-kinds.yaml.\n",
		total, counts["concurrency"], counts["contract"], counts["security"], counts["control"])
	fmt.Fprintf(&b, "hidden_tests:\n  - from: testdata/%s\n    to: %s\n", filepath.Base(testFile), filepath.ToSlash(testFile))
	b.WriteString("\nload:\n  script: k6/load.js\n  vus: 10\n  duration: 10s\n  rounds: 3\n")
	return b.String()
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}
