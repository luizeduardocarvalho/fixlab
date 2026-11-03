// Command fixlab evaluates a candidate change against an eval task.
//
//	fixlab run   --task tasks/x/task.yaml [--runs 3] [--candidate-ref REF | --candidate-patch FILE]
//	fixlab run   --task … --agent claude [--model M] [--prompt prompts/x.md] [--claude-md FILE|none] [--agent-plugin DIR] [--experiment NAME]
//	fixlab run   --task … --strategy strategies/<name>.yaml
//	fixlab history [--csv] [--task ID]
//	fixlab report  [-o report.html]
//	fixlab refine  -id ID -issue issue.md -base COMMIT [-claude-md FILE]
//	fixlab write-tests -task-dir tasks/ID -base COMMIT -test-file handlers/x_test.go
//	fixlab promote -task tasks/ID/task.yaml -result results/ID/RUN.json -branch fix/N-name
//	fixlab controls -task tasks/ID/task.yaml [-n 2]
//	fixlab failures [-task ID] [-variant NAME]
//	fixlab bench   [-strategy NAME] [-runs N]
//	fixlab compare -strategy NAME [-against NAME]
//	fixlab env up|down|check -project DIR
//	fixlab clean
//
// Every command but env works in a workspace (-workspace, default the
// current directory): the dir holding a project's tasks/, strategies/,
// prompts/ and benchmark.yaml.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/luizeduardocarvalho/fixlab/internal/budget"
	"github.com/luizeduardocarvalho/fixlab/internal/implement"
	"github.com/luizeduardocarvalho/fixlab/internal/profile"
	"github.com/luizeduardocarvalho/fixlab/internal/report"
	"github.com/luizeduardocarvalho/fixlab/internal/runner"
	"github.com/luizeduardocarvalho/fixlab/internal/strategy"
	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(dispatch(ctx, os.Args[1:]))
}

func dispatch(ctx context.Context, args []string) int {
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "run":
		return runCmd(ctx, args)
	case "clean":
		return cleanCmd(ctx, args)
	case "history":
		return historyCmd(args)
	case "report":
		return reportCmd(args)
	case "finalise":
		return finaliseCmd(ctx, args)
	case "review-tests":
		return reviewTestsCmd(ctx, args)
	case "refine":
		return refineCmd(ctx, args)
	case "write-tests":
		return writeTestsCmd(ctx, args)
	case "promote":
		return promoteCmd(ctx, args)
	case "controls":
		return controlsCmd(ctx, args)
	case "failures":
		return failuresCmd(args)
	case "bench":
		return benchCmd(ctx, args)
	case "compare":
		return compareCmd(args)
	case "env":
		return envCmd(ctx, args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (want run, clean, history, report, refine, finalise, write-tests, review-tests, promote, controls, failures, bench, compare or env)\n", cmd)
		return 2
	}
}

type common struct {
	workspace, apiRepo, webRepo, workDir, outDir string
}

func (c *common) register(fs *flag.FlagSet) {
	cwd, _ := os.Getwd()
	fs.StringVar(&c.workspace, "workspace", cwd, "workspace dir: the project's tasks/, strategies/, prompts/ and benchmark.yaml")
	fs.StringVar(&c.apiRepo, "api-repo", "", "repo that tasks with repo: api change (default: git toplevel of -workspace)")
	fs.StringVar(&c.webRepo, "web-repo", "", "repo that tasks with repo: web change (required for those tasks)")
	fs.StringVar(&c.workDir, "work", "", "dir for per-run worktrees (default: <workspace>/.work)")
	fs.StringVar(&c.outDir, "out", "", "results dir (default: <workspace>/results)")
}

func (c *common) config() (runner.Config, error) {
	var cfg runner.Config
	workspace, err := filepath.Abs(c.workspace)
	if err != nil {
		return cfg, err
	}
	if st, err := os.Stat(filepath.Join(workspace, "tasks")); err != nil || !st.IsDir() {
		return cfg, fmt.Errorf("%s does not look like a workspace: it has no tasks/ dir", workspace)
	}
	apiRepo := c.apiRepo
	if apiRepo == "" {
		out, err := exec.Command("git", "-C", workspace, "rev-parse", "--show-toplevel").Output()
		if err != nil {
			return cfg, fmt.Errorf("find api repo (pass -api-repo): %w", err)
		}
		apiRepo = strings.TrimSpace(string(out))
	}
	webRepo := c.webRepo
	if webRepo != "" {
		if webRepo, err = filepath.Abs(webRepo); err != nil {
			return cfg, err
		}
	}
	cfg = runner.Config{
		APIRepo:   apiRepo,
		WebRepo:   webRepo,
		Workspace: workspace,
		WorkDir:   firstNonEmpty(c.workDir, filepath.Join(workspace, ".work")),
		OutDir:    firstNonEmpty(c.outDir, filepath.Join(workspace, "results")),
		Logf:      func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	}
	cfg.WorkDir, _ = filepath.Abs(cfg.WorkDir)
	cfg.OutDir, _ = filepath.Abs(cfg.OutDir)
	return cfg, nil
}

func runCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var c common
	c.register(fs)
	taskPath := fs.String("task", "", "task YAML file (required)")
	runs := fs.Int("runs", 1, "number of independent runs")
	ref := fs.String("candidate-ref", "", "git ref whose diff from base_commit is the candidate")
	patch := fs.String("candidate-patch", "", "patch file (git diff format) to apply as the candidate")
	goBin := fs.String("go", firstNonEmpty(os.Getenv("GO"), "go"), "go binary")
	p95Tol := fs.Float64("p95-tolerance", 0.25, "allowed relative p95 regression (0.25 = +25%)")
	p95Slack := fs.Float64("p95-slack-ms", 5, "absolute p95 slack in ms on top of -p95-tolerance")
	errTol := fs.Float64("error-rate-tolerance", 0.001, "allowed absolute increase in 5xx rate, and drop in k6 expected-status rate")
	infraRetries := fs.Int("infra-retries", 2, "times to grade again (same candidate diff) after an infra error")

	regrade := fs.String("regrade", "", "recorded attempt dir (<history>/attempts/<task>/<run_id>) whose diff to grade again, keeping its agent fields")
	agentName := fs.String("agent", "", `have an agent implement the task ("claude"); overrides the candidate flags`)
	model := fs.String("model", "", "agent model (default: the CLI's default)")
	effort := fs.String("effort", "", "agent effort level: low, medium, high, xhigh, max (default: CLI default)")
	prompt := fs.String("prompt", "prompts/issue-v1.md", "agent prompt template, relative to -workspace")
	claudeMD := fs.String("claude-md", "", `CLAUDE.md the agent sees: "" = the repo's, "none", or a file to use instead`)
	agentCLI := fs.String("agent-cli", firstNonEmpty(os.Getenv("CLAUDE_BIN"), "claude"), "claude binary (env CLAUDE_BIN)")
	agentConfig := fs.String("agent-config-dir", os.Getenv("CLAUDE_CONFIG_DIR"), "CLAUDE_CONFIG_DIR for the agent's account")
	budget := fs.Float64("agent-budget-usd", 5, "per-attempt limit passed to the CLI as --max-budget-usd, at API list price; on a subscription it caps usage, not billing (0 = none)")
	agentTimeout := fs.Duration("agent-timeout", 30*time.Minute, "per-attempt agent time limit")
	var plugins, extraTools, ignore []string
	setup := fs.String("agent-setup", "", "shell command run in the agent's snapshot before it starts, e.g. \"graphify update .\"")
	fs.Func("agent-ignore", "path git ignores in the agent's snapshot, kept out of the diff (repeatable), e.g. graphify-out/", func(v string) error {
		ignore = append(ignore, v)
		return nil
	})
	fs.Func("agent-allow", `extra tool the agent may use, e.g. "Task" for subagents (repeatable)`, func(v string) error {
		extraTools = append(extraTools, v)
		return nil
	})
	fs.Func("agent-plugin", "plugin dir to load into the agent session (repeatable); user-installed plugins never load", func(v string) error {
		abs, err := filepath.Abs(v)
		if err != nil {
			return err
		}
		plugins = append(plugins, abs)
		return nil
	})

	strategyPath := fs.String("strategy", "", "strategy file (strategies/<name>.yaml) setting every agent option at once; excludes the individual agent flags")
	experiment := fs.String("experiment", "", "label grouping these attempts in the history, e.g. baseline or claude-md-v2")
	history := fs.String("history", "", "history repo checkout to record attempts in (default: env "+historyEnv+")")
	noHistory := fs.Bool("no-history", false, "don't record attempts in the history repo")
	historyCommit := fs.Bool("history-commit", true, "commit each attempt in the history repo (never pushes)")
	pastCap := fs.String("past-cap", "", "run agent attempts past the attempt cap (see internal/budget); the reason is recorded in each attempt")
	_ = fs.Parse(args)

	if *taskPath == "" || *runs < 1 {
		fs.Usage()
		return 2
	}
	var strat strategy.Strategy
	if *strategyPath != "" {
		// A strategy is the whole agent configuration: mixing in individual
		// flags would make the recorded strategy lie about what ran.
		var mixed []string
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "agent", "model", "effort", "prompt", "claude-md", "agent-plugin", "agent-allow",
				"agent-setup", "agent-ignore", "agent-budget-usd", "agent-timeout":
				mixed = append(mixed, "-"+f.Name)
			}
		})
		if len(mixed) > 0 {
			fmt.Fprintf(os.Stderr, "-strategy sets every agent option; drop %s or the strategy\n", strings.Join(mixed, " "))
			return 2
		}
		var err error
		if strat, err = strategy.Read(*strategyPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	t, err := task.Read(*taskPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	cfg, err := c.config()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	cfg.Experiment, cfg.HistoryCommit = *experiment, *historyCommit
	if strat.Name != "" {
		*agentName, *model, *effort = "claude", strat.Model, strat.Effort
		*prompt, *claudeMD, *setup = strategy.Resolve(strat.Prompt, cfg.Workspace), strategy.Resolve(strat.ClaudeMD, cfg.Workspace), strat.Setup
		for _, p := range strat.Plugins {
			plugins = append(plugins, strategy.Resolve(p, cfg.Workspace))
		}
		extraTools, ignore = strat.Allow, strat.Ignore
		if strat.BudgetUSD > 0 {
			*budget = strat.BudgetUSD
		}
		if strat.Timeout != "" {
			*agentTimeout, _ = time.ParseDuration(strat.Timeout)
		}
		cfg.Strategy, cfg.StrategySHA = strat.Name, strat.SHA256
		if cfg.Experiment == "" {
			cfg.Experiment = strat.Name
		}
	}
	if !*noHistory {
		cfg.HistoryDir = resolveHistory(*history)
	}
	p, cleanup, err := taskProfile(ctx, cfg, t)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer cleanup()
	cfg.Profile = p

	var impl implement.Implementer
	switch {
	case *regrade != "":
		g := implement.Regrade{AttemptDir: *regrade}
		runID, taskID, err := g.Source()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if taskID != t.ID {
			fmt.Fprintf(os.Stderr, "attempt %s is for task %s, not %s\n", runID, taskID, t.ID)
			return 2
		}
		impl, cfg.RegradedFrom = g, runID
	case *agentName == "":
		if impl, err = pickImplementer(t, *ref, *patch); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	case *agentName == "claude":
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
		// Everything that could reveal the answer: the repos and their
		// worktrees (reference fixes on other branches), the workspace
		// (hidden tests) and the history (other attempts' diffs).
		denied := deniedDirs(cfg, cfg.HistoryDir)
		agent := implement.Agent{
			CLI: *agentCLI, ConfigDir: *agentConfig, Model: *model, Effort: *effort,
			PromptPath: promptPath, ClaudeMD: mdPath, BudgetUSD: *budget, Timeout: *agentTimeout,
			ProjectTools: p.AgentTools(), AllowedTools: extraTools, DeniedPaths: denied, Strip: snapshotStrip(cfg), Plugins: plugins,
			Setup: *setup, Ignore: ignore,
		}
		impl = agent
		cfg.AgentAllowedTools, cfg.AgentDeniedPaths = agent.Tools(), len(denied)
	default:
		fmt.Fprintf(os.Stderr, "unknown agent %q (want claude)\n", *agentName)
		return 2
	}
	agent, isAgent := impl.(implement.Agent)
	switch {
	case cfg.HistoryDir != "":
		fmt.Fprintf(os.Stderr, "recording attempts in %s\n", cfg.HistoryDir)
	case isAgent && !*noHistory:
		// The attempt cap counts recorded attempts: an agent run that records
		// nowhere would never reach it.
		fmt.Fprintf(os.Stderr, "agent runs need a history for the attempt cap: pass -history, set %s, or pass -no-history\n", historyEnv)
		return 2
	case !*noHistory:
		fmt.Fprintf(os.Stderr, "not recording attempts: no -history or %s\n", historyEnv)
	}
	if isAgent {
		if code := checkBudget(ctx, &cfg, t, agent, *runs, *pastCap); code != 0 {
			return code
		}
	}
	cfg.GoBin = *goBin
	cfg.P95Tolerance, cfg.P95SlackMs, cfg.ErrorRateTolerance = *p95Tol, *p95Slack, *errTol
	cfg.InfraRetries = *infraRetries

	passed, failed, errored := 0, 0, 0
	for n := 1; n <= *runs; n++ {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "interrupted")
			return 130
		}
		fmt.Fprintf(os.Stderr, "== %s run %d/%d (%s)\n", t.ID, n, *runs, impl.Name())
		res, path := runner.Run(ctx, cfg, t, impl, n, *runs)
		var verdict string
		switch {
		case res.Pass:
			verdict = "PASS"
			passed++
		case res.HasInfraError():
			verdict = "ERROR at " + res.FailureStage + " (infrastructure, not a verdict; rerun)"
			errored++
		default:
			verdict = "FAIL at " + res.FailureStage
			failed++
		}
		fmt.Printf("%s run %d/%d: %s (%.0fs) → %s\n", t.ID, n, *runs, verdict, res.DurationSec, path)
	}
	fmt.Printf("%s: %d passed, %d failed, %d infrastructure errors (of %d runs)\n", t.ID, passed, failed, errored, *runs)
	if passed != *runs {
		return 1
	}
	return 0
}

// checkBudget refuses agent runs past the attempt cap, unless pastCap gives a
// reason, which every attempt then records. Without a history there is
// nothing to count, so the cap can't apply.
func checkBudget(ctx context.Context, cfg *runner.Config, t task.Task, agent implement.Agent, runs int, pastCap string) int {
	if cfg.HistoryDir == "" {
		return 0
	}
	prior, err := report.LoadAttempts(cfg.HistoryDir, t.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "attempt cap: read history: %v\n", err)
		return 2
	}
	repo, err := cfg.RepoFor(t.Repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "attempt cap: %v\n", err)
		return 2
	}
	promptSHA, _, _, err := implement.Fingerprint(ctx, agent.PromptPath, agent.ClaudeMD, agent.Plugins, repo, t)
	if err != nil {
		fmt.Fprintf(os.Stderr, "attempt cap: render prompt: %v\n", err)
		return 2
	}
	err = budget.Check(prior, cfg.Experiment, budget.Inputs{Prompt: promptSHA, Grading: t.GradingSHA256()}, runs)
	switch {
	case err == nil:
		return 0
	case pastCap != "":
		fmt.Fprintf(os.Stderr, "attempt cap: %v\nrunning anyway (-past-cap %q)\n", err, pastCap)
		cfg.CapOverride = pastCap
		return 0
	default:
		fmt.Fprintf(os.Stderr, "attempt cap: %v\n(pass -past-cap \"<reason>\" only if the maintainer agreed to go past it)\n", err)
		return 2
	}
}

// taskProfile resolves the project profile a task is graded with. Web tasks
// have none yet: they run the built-in web stages. cleanup removes the
// profile's extracted copy.
func taskProfile(ctx context.Context, cfg runner.Config, t task.Task) (*profile.Profile, func(), error) {
	if t.Repo == task.RepoWeb {
		return nil, func() {}, nil
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return nil, nil, err
	}
	dest, err := os.MkdirTemp(cfg.WorkDir, "profile-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(dest) }
	p, err := profile.Resolve(ctx, cfg.APIRepo, t.BaseCommit, dest)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if s := t.Load.Script; s != "" && p.Load != nil {
		if _, err := os.Stat(filepath.Join(p.Dir, s)); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("task %s: load.script: %w", t.ID, err)
		}
	}
	return p, cleanup, nil
}

func pickImplementer(t task.Task, ref, patch string) (implement.Implementer, error) {
	switch {
	case ref != "" && patch != "":
		return nil, errors.New("use only one of -candidate-ref and -candidate-patch")
	case patch != "":
		return implement.Patch{Path: patch}, nil
	case ref != "":
		return implement.Ref{Ref: ref}, nil
	case t.ReferenceCommit != "":
		return implement.Ref{Ref: t.ReferenceCommit}, nil
	default:
		return nil, errors.New("no candidate: pass -candidate-ref or -candidate-patch, or set reference_commit in the task")
	}
}

func cleanCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("clean", flag.ExitOnError)
	var c common
	c.register(fs)
	_ = fs.Parse(args)
	cfg, err := c.config()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := runner.Clean(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// historyEnv names the history repo when -history doesn't.
const historyEnv = "FIXLAB_HISTORY"

// resolveHistory picks the history dir: the flag value, else the
// FIXLAB_HISTORY env var, else none.
func resolveHistory(flagValue string) string {
	v := firstNonEmpty(flagValue, os.Getenv(historyEnv))
	if v == "" {
		return ""
	}
	abs, _ := filepath.Abs(v)
	return abs
}

// deniedDirs is every dir an agent session must not read: the parents of the
// repos (so their worktrees too), the workspace and the given extras.
func deniedDirs(cfg runner.Config, extra ...string) []string {
	dirs := []string{filepath.Dir(cfg.APIRepo), cfg.Workspace}
	if cfg.WebRepo != "" {
		dirs = append(dirs, filepath.Dir(cfg.WebRepo))
	}
	return uniqueDirs(append(dirs, extra...)...)
}

// snapshotStrip is the workspace's path inside its git checkout, removed
// from every agent snapshot: a workspace kept in the project's own repo has
// tasks, hidden tests and reports in the snapshot's commit too. A workspace
// outside any repo, or at a repo's root, strips nothing.
func snapshotStrip(cfg runner.Config) []string {
	out, err := exec.Command("git", "-C", cfg.Workspace, "rev-parse", "--show-prefix").Output()
	prefix := strings.TrimSuffix(strings.TrimSpace(string(out)), "/")
	if err != nil || prefix == "" {
		return nil
	}
	return []string{prefix}
}

func uniqueDirs(dirs ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range dirs {
		if d == "" {
			continue
		}
		d = filepath.Clean(d)
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
