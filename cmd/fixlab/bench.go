package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/luizeduardocarvalho/fixlab/internal/bench"
	"github.com/luizeduardocarvalho/fixlab/internal/report"
	"github.com/luizeduardocarvalho/fixlab/internal/runner"
	"github.com/luizeduardocarvalho/fixlab/internal/strategy"
	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

// loadBench reads the workspace's benchmark.yaml and every strategy file.
func loadBench(evalDir string) (*bench.Benchmark, []strategy.Strategy, error) {
	b, err := bench.Read(filepath.Join(evalDir, "benchmark.yaml"))
	if err != nil {
		return nil, nil, err
	}
	files, err := filepath.Glob(filepath.Join(evalDir, "strategies", "*.yaml"))
	if err != nil {
		return nil, nil, err
	}
	// The default first, so it wins when two strategies are the same config.
	var out []strategy.Strategy
	for _, f := range files {
		s, err := strategy.Read(f)
		if err != nil {
			return nil, nil, err
		}
		if s.Name == b.Default {
			out = append([]strategy.Strategy{s}, out...)
		} else {
			out = append(out, s)
		}
	}
	return &b, out, nil
}

// buildData builds the report data with the benchmark comparisons.
func buildData(cfg runner.Config, root string) (report.Data, error) {
	in := report.Input{
		HistoryDir: root, TasksDir: filepath.Join(cfg.Workspace, "tasks"),
		Repos:          map[string]string{task.RepoAPI: cfg.APIRepo, task.RepoWeb: cfg.WebRepo},
		HarnessVersion: runner.HarnessVersion(), Now: time.Now(), Workspace: cfg.Workspace,
	}
	if b, strategies, err := loadBench(cfg.Workspace); err == nil {
		in.Benchmark, in.Strategies = b, strategies
	} else {
		fmt.Fprintln(os.Stderr, "warning: no benchmark comparisons:", err)
	}
	return report.Build(in)
}

// benchCmd runs one strategy on every benchmark task.
func benchCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	var c common
	c.register(fs)
	name := fs.String("strategy", "", "strategy name, <workspace>/strategies/<name>.yaml (default: the benchmark's default)")
	runs := fs.Int("runs", 0, "runs per task (default: the benchmark's runs)")
	goBin := fs.String("go", firstNonEmpty(os.Getenv("GO"), "go"), "go binary")
	dryRun := fs.Bool("dry-run", false, "print what would run, run nothing")
	_ = fs.Parse(args)

	cfg, err := c.config()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	b, _, err := loadBench(cfg.Workspace)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	strat := firstNonEmpty(*name, b.Default)
	path := filepath.Join(cfg.Workspace, "strategies", strat+".yaml")
	s, err := strategy.Read(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	n := b.Runs
	if *runs > 0 {
		n = *runs
	}
	prompt, _ := os.ReadFile(strategy.Resolve(s.Prompt, cfg.Workspace))
	needsRefined := strings.Contains(string(prompt), ".Refined")

	failed := 0
	for _, tp := range b.Tasks {
		full := filepath.Join(cfg.Workspace, tp)
		t, err := task.Read(full)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			failed++
			continue
		}
		if needsRefined && strings.TrimSpace(t.Refined) == "" {
			fmt.Printf("== %s: skipped, %s needs the task's refined.md (run fixlab refine and record the decisions)\n", t.ID, strat)
			continue
		}
		fmt.Printf("== %s: %s, %d runs\n", t.ID, strat, n)
		if *dryRun {
			continue
		}
		if code := runCmd(ctx, []string{"-task", full, "-runs", strconv.Itoa(n), "-strategy", path, "-go", *goBin,
			"-workspace", cfg.Workspace, "-api-repo", cfg.APIRepo, "-web-repo", cfg.WebRepo}); code != 0 {
			failed++
		}
		if ctx.Err() != nil {
			return 130
		}
	}
	if *dryRun {
		fmt.Println("dry run: nothing ran")
		return 0
	}
	fmt.Printf("bench %s: %d of %d tasks had a run that didn't pass; see fixlab failures and fixlab compare -strategy %s\n", strat, failed, len(b.Tasks), strat)
	return 0
}

// compareCmd compares a strategy with the benchmark's default and applies
// the adoption rule.
func compareCmd(args []string) int {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	var c common
	c.register(fs)
	name := fs.String("strategy", "", "strategy to compare (required)")
	against := fs.String("against", "", "strategy to compare with (default: the benchmark's default)")
	dir := fs.String("history", "", "history repo checkout (default: env "+historyEnv+")")
	_ = fs.Parse(args)
	if *name == "" {
		fs.Usage()
		return 2
	}
	cfg, err := c.config()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	b, _, err := loadBench(cfg.Workspace)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	root := resolveHistory(*dir)
	d, err := buildData(cfg, root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	def := firstNonEmpty(*against, b.Default)
	var tasks []string
	if d.Benchmark != nil {
		tasks = d.Benchmark.Tasks
	}
	cmp := report.Compare(d, *name, def, tasks, report.CompareRule{MinRuns: b.AdoptIf.MinRuns, MaxTokenIncrease: b.AdoptIf.MaxTokenIncrease})

	fmt.Printf("%s vs %s (default) on %d benchmark tasks\n\n", *name, def, len(tasks))
	fmt.Printf("%-28s %-24s %-24s %-14s %s\n", "TASK", "PASS (default → new)", "HIDDEN SHARE", "SECURITY", "TOKENS/RUN")
	for _, t := range cmp.Tasks {
		fmt.Printf("%-28s %-24s %-24s %-14s %s\n", t.TaskID,
			fmt.Sprintf("%d/%d → %d/%d", t.Default.Passes, t.Default.Runs, t.Candidate.Passes, t.Candidate.Runs),
			fmt.Sprintf("%.0f%% → %.0f%%", 100*t.Default.HiddenShare, 100*t.Candidate.HiddenShare),
			fmt.Sprintf("%d/%d → %d/%d", t.Default.SecurityPass, t.Default.SecurityRuns, t.Candidate.SecurityPass, t.Candidate.SecurityRuns),
			fmt.Sprintf("%.2fM → %.2fM", t.Default.TokensAvg/1e6, t.Candidate.TokensAvg/1e6))
	}
	fmt.Printf("\nVerdict: %s\n", cmp.Verdict)
	for _, r := range cmp.Reasons {
		fmt.Printf("  - %s\n", r)
	}
	return 0
}
