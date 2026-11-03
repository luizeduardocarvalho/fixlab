package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/luizeduardocarvalho/fixlab/internal/profile"
	"github.com/luizeduardocarvalho/fixlab/internal/result"
	pkgstack "github.com/luizeduardocarvalho/fixlab/internal/stack"
)

// ProjectPrefix starts every compose project name the harness creates; the
// clean command removes any project with this prefix.
const ProjectPrefix = "fixlab-"

type stack struct {
	role     string // "baseline" or "candidate"
	project  string
	worktree string
	out      string
	file     string // rendered compose file
}

type loadSummary struct {
	P95Ms     float64 `json:"p95_ms"`
	ErrorRate float64 `json:"error_rate"`
	Requests  int     `json:"requests"`
	Checks    float64 `json:"checks_pass_rate"`
}

func (r *run) compose(st stack, log string, timeout time.Duration, args ...string) cmdSpec {
	return cmdSpec{
		log:     log,
		timeout: timeout,
		name:    "docker",
		args:    append([]string{"compose", "-f", st.file, "-p", st.project}, args...),
	}
}

// downStack removes a stack's containers, network and locally built image.
// It uses its own context so it still runs after cancellation.
func (r *run) downStack(st stack) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := r.exec(ctx, r.compose(st, "compose-down-"+st.role, 0, "down", "-v", "--remove-orphans", "--rmi", "local", "--timeout", "5")); err != nil {
		r.logf("cleanup: %v", err)
	}
}

// perf brings up baseline and candidate stacks, seeds both, runs the same k6
// script against each, and compares p95 latency and 5xx rate.
func (r *run) perf(ctx context.Context) {
	p := r.cfg.Profile
	outs, err := pkgstack.Prepare(p, r.assets)
	if err != nil {
		r.infraError(&r.res.StageStackUp, "stack_up", err)
		return
	}
	stacks := []stack{
		{role: "baseline", project: r.project("base"), worktree: r.baseWT, out: filepath.Join(r.artifacts, "k6-baseline")},
		{role: "candidate", project: r.project("cand"), worktree: r.candWT, out: filepath.Join(r.artifacts, "k6-candidate")},
	}
	for i := range stacks {
		st := &stacks[i]
		if err := os.MkdirAll(st.out, 0o755); err != nil {
			r.infraError(&r.res.StageStackUp, "stack_up", err)
			return
		}
		file, err := pkgstack.Compose(pkgstack.Spec{Profile: p, Worktree: st.worktree, Assets: r.assets, Out: st.out}, outs)
		if err != nil {
			r.infraError(&r.res.StageStackUp, "stack_up", err)
			return
		}
		st.file = filepath.Join(filepath.Dir(r.assets), st.role+"-compose.yml")
		if err := os.WriteFile(st.file, file, 0o644); err != nil {
			r.infraError(&r.res.StageStackUp, "stack_up", err)
			return
		}
		r.onCleanup(func() { r.downStack(*st) })
	}

	// Bring both stacks up concurrently; they share nothing but the host.
	errs := make([]error, len(stacks))
	up := append([]string{"up", "-d", "--build", "--wait", "--wait-timeout", "600"}, pkgstack.InfraServices(p)...)
	var wg sync.WaitGroup
	for i, st := range stacks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.exec(ctx, r.compose(st, "compose-up-"+st.role, 15*time.Minute, up...))
		}()
	}
	wg.Wait()
	if !r.stackStep(&r.res.StageStackUp, "stack_up", errs) {
		return
	}

	if p.Seed != nil {
		sql, err := os.ReadFile(filepath.Join(p.Dir, p.Seed.SQL))
		if err != nil {
			r.infraError(&r.res.StageSeed, "seed", err)
			return
		}
		for i, st := range stacks {
			_, errs[i] = r.exec(ctx, withStdin(r.compose(st, "seed-"+st.role, 5*time.Minute, "run", "--rm", "-T", pkgstack.SeedService), string(sql)))
		}
		if !r.stackStep(&r.res.StageSeed, "seed", errs) {
			return
		}
	}

	// Stacks may run in parallel, but load tests must not: concurrent k6 runs
	// compete for the same CPU and make the comparison meaningless.
	unlock, err := r.lockLoad()
	if err != nil {
		r.infraError(&r.res.StageLoadTest, "load_test", err)
		return
	}
	defer unlock()

	// A failing verdict is confirmed with a second, fresh load test before it
	// counts: one noisy window on a shared machine must not fail a candidate.
	for try := 1; ; try++ {
		rounds, ok := r.loadRounds(ctx, stacks, errs, try)
		if !ok {
			return
		}
		problems, ok := r.judgePerf(rounds)
		if !ok {
			return
		}
		if len(problems) == 0 {
			r.res.StagePerf = result.Pass
			return
		}
		if try == 2 {
			r.fail(&r.res.StagePerf, "perf", fmt.Errorf("%s (confirmed by a second load test)", strings.Join(problems, "; ")))
			return
		}
		r.res.PerfReruns = 1
		r.logf("perf: %s; confirming with a second load test", strings.Join(problems, "; "))
	}
}

// loadRounds runs the task's k6 rounds on both stacks, interleaved (A B B A
// A B …) so a transient spike on the host lands in one round of one side
// instead of deciding the comparison. Even runs start with the candidate.
// Summaries go to summary-round<N>.json (summary-try<T>-round<N>.json after
// the first try).
func (r *run) loadRounds(ctx context.Context, stacks []stack, errs []error, try int) ([][]loadSummary, bool) {
	load := r.load()
	script := pkgstack.LoadScript(load.Script)
	rounds := make([][]loadSummary, len(stacks))
	for round := 0; round < load.Rounds; round++ {
		order := []int{0, 1}
		if (round+r.n)%2 == 0 {
			order = []int{1, 0}
		}
		for _, i := range order {
			st := stacks[i]
			_, errs[i] = r.exec(ctx, r.compose(st, "k6-"+st.role, 10*time.Minute, "run", "--rm", "-T",
				"-e", fmt.Sprintf("VUS=%d", load.VUs), "-e", "DURATION="+load.Duration,
				pkgstack.LoadService, "run", "--quiet", script))
			if errs[i] != nil {
				break
			}
			name := fmt.Sprintf("summary-round%d.json", round+1)
			if try > 1 {
				name = fmt.Sprintf("summary-try%d-round%d.json", try, round+1)
			}
			path := filepath.Join(st.out, name)
			if errs[i] = os.Rename(filepath.Join(st.out, "summary.json"), path); errs[i] != nil {
				break
			}
			var s loadSummary
			if s, errs[i] = readLoadSummary(path); errs[i] != nil {
				break
			}
			rounds[i] = append(rounds[i], s)
		}
		if errs[0] != nil || errs[1] != nil {
			break
		}
	}
	return rounds, r.stackStep(&r.res.StageLoadTest, "load_test", errs)
}

// judgePerf records the combined numbers and returns what the candidate
// regressed. ok is false when the window can't be judged (an infra error is
// recorded).
func (r *run) judgePerf(rounds [][]loadSummary) (problems []string, ok bool) {
	base, cand := combine(rounds[0]), combine(rounds[1])
	delta := 0.0
	if base.P95Ms > 0 {
		delta = (cand.P95Ms - base.P95Ms) / base.P95Ms * 100
	}
	r.res.BaselineP95Ms, r.res.CandidateP95Ms, r.res.P95DeltaPct = ptr(round(base.P95Ms)), ptr(round(cand.P95Ms)), ptr(round(delta))
	r.res.BaselineErrorRate, r.res.CandidateErrorRate = ptr(base.ErrorRate), ptr(cand.ErrorRate)
	r.res.BaselineRequests, r.res.CandidateRequests = base.Requests, cand.Requests
	r.res.BaselineChecksRate, r.res.CandidateChecksRate = ptr(base.Checks), ptr(cand.Checks)

	// The baseline is known-good: if one of its rounds got far fewer requests
	// than its others, the host stalled and no round in this window can be
	// compared. A slow candidate alone is still the candidate's.
	if i, stalled := stalledRound(rounds[0]); stalled {
		r.infraError(&r.res.StagePerf, "perf", fmt.Errorf("host stalled during the load test: baseline round %d completed %d requests (best round %d); rerun",
			i+1, rounds[0][i].Requests, maxRequests(rounds[0])))
		return nil, false
	}

	if cand.ErrorRate > base.ErrorRate+r.cfg.ErrorRateTolerance {
		problems = append(problems, fmt.Sprintf("5xx rate %.4f > baseline %.4f (+%.4f allowed)", cand.ErrorRate, base.ErrorRate, r.cfg.ErrorRateTolerance))
	}
	if cand.Checks < base.Checks-r.cfg.ErrorRateTolerance {
		problems = append(problems, fmt.Sprintf("expected-status rate %.4f < baseline %.4f (-%.4f allowed)", cand.Checks, base.Checks, r.cfg.ErrorRateTolerance))
	}
	if limit := base.P95Ms*(1+r.cfg.P95Tolerance) + r.cfg.P95SlackMs; cand.P95Ms > limit {
		problems = append(problems, fmt.Sprintf("p95 %.1fms > limit %.1fms (baseline %.1fms +%.0f%% +%.0fms)", cand.P95Ms, limit, base.P95Ms, r.cfg.P95Tolerance*100, r.cfg.P95SlackMs))
	}
	return problems, true
}

// stackStep records a step that ran on both stacks. A baseline failure is an
// infrastructure error (base_commit is known-good); a candidate failure is
// the candidate's.
func (r *run) stackStep(stage *string, name string, errs []error) bool {
	if errs[0] != nil {
		r.infraError(stage, name, fmt.Errorf("baseline: %w", errs[0]))
		return false
	}
	if errs[1] != nil {
		r.fail(stage, name, fmt.Errorf("candidate: %w", errs[1]))
		return false
	}
	*stage = result.Pass
	return true
}

// lockLoad takes an exclusive lock shared by every harness process on the
// machine, whatever its checkout or work dir, blocking until it is free:
// two sessions' load tests running at once would skew each other's numbers.
func (r *run) lockLoad() (func(), error) {
	path, err := loadLockPath()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		r.logf("waiting for another run's load test to finish")
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return nil, err
		}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// load is the profile's load settings with the task's overrides applied.
// Scripts are relative to the profile dir.
func (r *run) load() profile.Load {
	l := *r.cfg.Profile.Load
	t := r.task.Load
	if t.Script != "" {
		l.Script = t.Script
	}
	if t.VUs > 0 {
		l.VUs = t.VUs
	}
	if t.Duration != "" {
		l.Duration = t.Duration
	}
	if t.Rounds > 0 {
		l.Rounds = t.Rounds
	}
	return l
}

// loadLockPath is the machine-wide load-test lock, in the user's cache dir.
func loadLockPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "fixlab")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "load-test.lock"), nil
}

func (r *run) project(role string) string {
	return ProjectPrefix + r.id + "-" + role
}

func withStdin(spec cmdSpec, input string) cmdSpec {
	spec.stdin = strings.NewReader(input)
	return spec
}

func readLoadSummary(path string) (loadSummary, error) {
	var s loadSummary
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.Requests == 0 {
		return s, fmt.Errorf("load test made no requests")
	}
	return s, nil
}

// combine merges a stack's rounds: p95 is the median of the per-round p95s;
// rates are request-weighted over all rounds.
func combine(rounds []loadSummary) loadSummary {
	var out loadSummary
	p95s := make([]float64, 0, len(rounds))
	var errs, checks float64
	for _, s := range rounds {
		p95s = append(p95s, s.P95Ms)
		out.Requests += s.Requests
		errs += s.ErrorRate * float64(s.Requests)
		checks += s.Checks * float64(s.Requests)
	}
	sort.Float64s(p95s)
	if n := len(p95s); n%2 == 1 {
		out.P95Ms = p95s[n/2]
	} else if n > 0 {
		out.P95Ms = (p95s[n/2-1] + p95s[n/2]) / 2
	}
	if out.Requests > 0 {
		out.ErrorRate = errs / float64(out.Requests)
		out.Checks = checks / float64(out.Requests)
	}
	return out
}

// stalledRound reports the first round that completed under a quarter of the
// requests of the best round.
func stalledRound(rounds []loadSummary) (int, bool) {
	best := maxRequests(rounds)
	for i, s := range rounds {
		if s.Requests*4 < best {
			return i, true
		}
	}
	return 0, false
}

func maxRequests(rounds []loadSummary) int {
	best := 0
	for _, s := range rounds {
		best = max(best, s.Requests)
	}
	return best
}

func ptr(v float64) *float64 { return &v }

func round(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }
