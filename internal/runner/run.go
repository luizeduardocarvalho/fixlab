// Package runner executes one eval run: worktrees → candidate → static
// checks → tests → baseline-vs-candidate load test → teardown.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luizeduardocarvalho/fixlab/internal/implement"
	"github.com/luizeduardocarvalho/fixlab/internal/profile"
	"github.com/luizeduardocarvalho/fixlab/internal/result"
	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

// Config is shared by all runs of an invocation.
type Config struct {
	APIRepo   string // repo that tasks with repo: api change
	WebRepo   string // repo that tasks with repo: web change; "" = none given
	Workspace string // absolute path of the workspace: tasks/, strategies/, prompts/
	WorkDir   string // parent dir for per-run worktrees and assets
	OutDir    string // results root

	// Profile is the project's eval profile for the task's base commit; nil
	// for web tasks, which still use the built-in web stages.
	Profile *profile.Profile
	GoBin   string // go binary; its dir goes first on PATH for stage commands

	Experiment    string // free-form label grouping attempts, e.g. "claude-md-v2"
	Strategy      string // strategy file name, "" if none
	StrategySHA   string // hash of the strategy file
	RegradedFrom  string // run_id of the attempt being regraded, if any
	CapOverride   string // why the run goes past the attempt cap, if it does
	HistoryDir    string // history repo checkout; "" = don't record history
	HistoryCommit bool   // commit each attempt in HistoryDir (never pushes)

	// Recorded in each attempt's manifest.
	AgentAllowedTools []string
	AgentDeniedPaths  int

	// InfraRetries is how many times grading is retried after an infra error
	// that happened once the candidate was applied. Retries replay the
	// candidate's diff; the implementer never runs twice.
	InfraRetries int

	P95Tolerance       float64 // allowed relative p95 regression, 0.25 = +25%
	P95SlackMs         float64 // absolute p95 slack on top of the tolerance
	ErrorRateTolerance float64 // allowed absolute increase in 5xx rate (and drop in k6 checks rate)

	Logf func(format string, args ...any)
}

// RepoFor is the checkout a task with the given repo: field runs in.
func (c Config) RepoFor(repo string) (string, error) {
	if repo != task.RepoWeb {
		return c.APIRepo, nil
	}
	if c.WebRepo == "" {
		return "", fmt.Errorf("a repo: %s task needs -web-repo", task.RepoWeb)
	}
	return c.WebRepo, nil
}

type run struct {
	cfg  Config
	task task.Task
	impl implement.Implementer
	n    int
	res  *result.Result

	total   int
	started string // RFC 3339 start of the first try

	id        string
	baseWT    string
	candWT    string
	assets    string
	artifacts string
	cleanups  []func()
	info      implement.Info // what the implementer reported, kept for retries
}

// Run executes run n (1-based) of total and writes its result JSON. It always
// tears down every worktree and compose stack it created before returning.
func Run(ctx context.Context, cfg Config, t task.Task, impl implement.Implementer, n, total int) (*result.Result, string) {
	r := &run{cfg: cfg, task: t, impl: impl, n: n, total: total}
	r.id = fmt.Sprintf("%s-r%d-%s", t.ID, n, randomSuffix())
	work := filepath.Join(cfg.WorkDir, r.id)
	r.baseWT, r.candWT, r.assets = filepath.Join(work, "base"), filepath.Join(work, "cand"), filepath.Join(work, "assets")
	r.artifacts = filepath.Join(cfg.OutDir, t.ID, r.id)
	start := time.Now()
	r.started = start.UTC().Format(time.RFC3339)
	r.res = r.freshResult()

	var retryErrors []string
	for try := 0; ; try++ {
		r.onCleanup(func() {
			if err := os.RemoveAll(work); err != nil {
				r.logf("cleanup: %v", err)
			}
		})
		func() {
			defer r.cleanup()
			executeRun(r, ctx)
		}()
		if try == cfg.InfraRetries || !r.retryable() || ctx.Err() != nil {
			break
		}
		retryErrors = append(retryErrors, fmt.Sprintf("%s: %s", r.res.FailureStage, r.res.Error))
		if r.res.StageApply == result.Error {
			// The session died on an API error before producing a candidate.
			r.logf("infra error at %s; running %s again (retry %d of %d)", r.res.FailureStage, impl.Name(), try+1, cfg.InfraRetries)
			r.impl = impl
		} else {
			r.logf("infra error at %s; grading again with the same candidate (retry %d of %d)", r.res.FailureStage, try+1, cfg.InfraRetries)
			r.impl = implement.Replay{DiffPath: filepath.Join(r.artifacts, "candidate.diff"), Info: r.info, As: impl.Name()}
		}
		r.res = r.freshResult()
	}
	r.res.InfraRetries = len(retryErrors)
	r.res.InfraRetryErrors = strings.Join(retryErrors, "\n")

	r.res.Pass = r.passed()
	r.res.DurationSec = round(time.Since(start).Seconds())
	path := filepath.Join(cfg.OutDir, t.ID, r.id+".json")
	if err := r.res.Write(path); err != nil {
		r.logf("write result: %v", err)
	}
	if err := r.saveHistory(path); err != nil {
		r.logf("history: %v", err)
	}
	return r.res, path
}

// executeRun is the stage pipeline; tests replace it to drive the retry loop
// without Docker.
var executeRun = func(r *run, ctx context.Context) { r.execute(ctx) }

// freshResult is a result with only the run's identity filled in.
func (r *run) freshResult() *result.Result {
	res := result.New()
	res.TaskID, res.Run, res.Runs, res.RunID = r.task.ID, r.n, r.total, r.id
	res.Area, res.TaskType, res.Repo = r.task.Area, r.task.TaskType, r.task.Repo
	res.Implementer, res.BaseCommit, res.Experiment = r.impl.Name(), r.task.BaseCommit, r.cfg.Experiment
	res.HarnessVersion = HarnessVersion()
	res.RegradedFrom = r.cfg.RegradedFrom
	res.TaskSHA256, res.GradingSHA256 = r.task.SHA256(), r.task.GradingSHA256()
	res.Strategy, res.StrategySHA256 = r.cfg.Strategy, r.cfg.StrategySHA
	res.CapOverride = r.cfg.CapOverride
	if p := r.cfg.Profile; p != nil {
		res.Profile, res.ProfileSHA256 = p.Name, p.SHA256
	}
	res.StartedAt = r.started
	return res
}

// retryable reports whether the run ended in an infra error after the
// candidate was applied, so grading can be repeated on the same diff.
// retryable: an infra error while grading (the candidate is kept and graded
// again), or an agent session that ended on a model API error, which runs
// the agent again. Other implementer failures are never retried: a session
// that crashed after real work would be paid for twice.
func (r *run) retryable() bool {
	if !r.res.HasInfraError() {
		return false
	}
	return r.res.StageApply == result.Pass || (r.res.StageApply == result.Error && r.info.APIError)
}

func (r *run) execute(ctx context.Context) {
	repo, err := r.cfg.RepoFor(r.task.Repo)
	if err != nil {
		r.infraError(&r.res.StageApply, "apply", err)
		return
	}
	// Without Docker the DB-backed tests skip and the stacks can't start. Check
	// before the implementer runs: an agent working without its tests is a
	// different experiment, and its attempt would be graded on nothing.
	if _, err := r.exec(ctx, cmdSpec{log: "preflight", name: "docker", args: []string{"info", "--format", "{{.ServerVersion}}"}, timeout: 30 * time.Second}); err != nil {
		r.infraError(&r.res.StageApply, "apply", fmt.Errorf("docker is not available: %w", err))
		return
	}
	for _, wt := range []string{r.baseWT, r.candWT} {
		if err := r.addWorktree(ctx, repo, wt); err != nil {
			r.infraError(&r.res.StageApply, "apply", err)
			return
		}
	}

	ws := implement.Workspace{Task: r.task, Repo: repo, Worktree: r.candWT, Artifacts: r.artifacts, GoBin: r.cfg.GoBin}
	info, err := r.impl.Implement(ctx, ws)
	r.info = info
	r.recordImplementer(info)
	if err != nil {
		var ie implement.InfraError
		if errors.As(err, &ie) {
			r.infraError(&r.res.StageApply, "apply", err)
		} else {
			r.fail(&r.res.StageApply, "apply", err)
		}
		return
	}
	if err := r.measureDiff(ctx, repo); err != nil {
		r.infraError(&r.res.StageApply, "apply", err)
		return
	}
	if err := r.copyHiddenTests(); err != nil {
		r.infraError(&r.res.StageApply, "apply", err)
		return
	}
	r.res.StageApply = result.Pass
	r.logf("apply: pass")

	if r.task.Repo == task.RepoWeb {
		r.webStages(ctx)
		r.logf("build: %s, lint: %s, typecheck: %s, vitest: %s (%d/%d passed), hidden tests: %s (%d/%d)",
			r.res.StageBuild, r.res.StageLint, r.res.StageTypecheck, r.res.StageFrontendTest,
			r.res.FrontendTestsPassed, r.res.FrontendTestsTotal, r.res.StageHiddenTests, r.res.HiddenTestsPassed, r.res.HiddenTestsTotal)
		return
	}

	ok := r.stages(ctx)
	r.logf("build: %s, vet: %s, lint: %s, test: %s (%d/%d passed), hidden tests: %s (%d/%d)",
		r.res.StageBuild, r.res.StageVet, r.res.StageLint, r.res.StageGoTest, r.res.GoTestsPassed, r.res.GoTestsTotal,
		r.res.StageHiddenTests, r.res.HiddenTestsPassed, r.res.HiddenTestsTotal)
	if !ok || !r.hasStack() {
		return
	}
	r.perf(ctx)
	r.logf("stack: %s, seed: %s, load: %s, perf: %s", r.res.StageStackUp, r.res.StageSeed, r.res.StageLoadTest, r.res.StagePerf)
}

// hasStack reports whether the profile defines an app to load-test.
func (r *run) hasStack() bool {
	return r.cfg.Profile.App != nil && r.cfg.Profile.Load != nil
}

// passed reports whether every stage that applies to the task's repo passed.
func (r *run) passed() bool {
	s := r.res
	required := []string{s.StageApply, s.StageHiddenTests}
	if r.task.Repo == task.RepoWeb {
		required = append(required, s.StageBuild, s.StageLint, s.StageTypecheck, s.StageFrontendTest)
	} else {
		for _, st := range r.cfg.Profile.Stages {
			field, _ := r.stageField(st.Name)
			required = append(required, *field)
		}
		if r.hasStack() {
			required = append(required, s.StageStackUp, s.StageLoadTest, s.StagePerf)
			if r.cfg.Profile.Seed != nil {
				required = append(required, s.StageSeed)
			}
		}
	}
	for _, st := range required {
		if st != result.Pass {
			return false
		}
	}
	return true
}

func (r *run) addWorktree(ctx context.Context, repo, path string) error {
	// Register removal first so a half-created worktree is still cleaned up.
	r.onCleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := os.Stat(path); err == nil {
			if _, err := r.exec(cctx, cmdSpec{log: "git-worktree", name: "git", args: []string{"-C", repo, "worktree", "remove", "--force", path}}); err != nil {
				r.logf("cleanup: %v", err)
			}
		}
		_, _ = r.exec(cctx, cmdSpec{log: "git-worktree", name: "git", args: []string{"-C", repo, "worktree", "prune"}})
	})
	_, err := r.exec(ctx, cmdSpec{log: "git-worktree", timeout: 2 * time.Minute, name: "git", args: []string{"-C", repo, "worktree", "add", "--detach", path, r.task.BaseCommit}})
	return err
}

func (r *run) copyHiddenTests() error {
	for _, h := range r.task.HiddenTests {
		src, err := os.ReadFile(filepath.Join(r.task.Dir, h.From))
		if err != nil {
			return err
		}
		dst := filepath.Join(r.candWT, h.To)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, src, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (r *run) onCleanup(fn func()) { r.cleanups = append(r.cleanups, fn) }

// cleanup runs registered teardown steps in reverse order, even if one panics.
func (r *run) cleanup() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		func() {
			defer func() {
				if p := recover(); p != nil {
					r.logf("cleanup panic: %v", p)
				}
			}()
			r.cleanups[i]()
		}()
	}
	r.cleanups = nil
}

// hostFailures are error fragments meaning the machine failed, whatever
// the stage: a full disk or a Docker VM whose filesystem went read-only must
// never be graded as the candidate's failure.
var hostFailures = []string{
	"no space left on device",
	"read-only file system",
	"input/output error",
}

// hostFailure returns the first host-failure fragment in s, or "".
func hostFailure(s string) string {
	for _, m := range hostFailures {
		if strings.Contains(s, m) {
			return m
		}
	}
	return ""
}

// fail marks a stage as failed by the candidate, unless the error shows the
// machine failed, which makes it an infra error.
func (r *run) fail(stage *string, name string, err error) {
	if m := hostFailure(err.Error()); m != "" {
		r.infraError(stage, name, fmt.Errorf("the machine failed (%s), not the candidate: %w", m, err))
		return
	}
	*stage = result.Fail
	r.record(name, err)
}

// infraError marks a stage as unable to produce a verdict.
func (r *run) infraError(stage *string, name string, err error) {
	*stage = result.Error
	r.record(name, err)
}

func (r *run) record(name string, err error) {
	r.logf("%s: %v", name, err)
	if r.res.FailureStage != "" {
		return
	}
	r.res.FailureStage = name
	msg := err.Error()
	if len(msg) > 2000 {
		msg = msg[:2000] + "…"
	}
	r.res.Error = msg
}

func (r *run) logf(format string, args ...any) {
	if r.cfg.Logf != nil {
		r.cfg.Logf("[%s] "+format, append([]any{r.id}, args...)...)
	}
}

func randomSuffix() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
