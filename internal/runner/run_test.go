package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/luizeduardocarvalho/fixlab/internal/implement"
	"github.com/luizeduardocarvalho/fixlab/internal/profile"
	"github.com/luizeduardocarvalho/fixlab/internal/result"
	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

type fakeImpl struct{}

func (fakeImpl) Name() string { return "claude:test" }
func (fakeImpl) Implement(context.Context, implement.Workspace) (implement.Info, error) {
	return implement.Info{Kind: "agent", Model: "m"}, nil
}

// repoProfile is the example project's profile.
func repoProfile(t *testing.T) *profile.Profile {
	t.Helper()
	p, err := profile.Read("../../testdata/example/.eval")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// stagedRun scripts what each try of the pipeline produces.
func stagedRun(t *testing.T, tries []func(r *run)) (*result.Result, []string) {
	t.Helper()
	var impls []string
	n := 0
	orig := executeRun
	executeRun = func(r *run, _ context.Context) {
		impls = append(impls, r.impl.Name()+"|"+typeName(r.impl))
		info, _ := r.impl.Implement(context.Background(), implement.Workspace{})
		r.info = info
		r.recordImplementer(info)
		tries[n](r)
		n++
	}
	t.Cleanup(func() { executeRun = orig })
	cfg := Config{WorkDir: t.TempDir(), OutDir: t.TempDir(), Workspace: t.TempDir(), InfraRetries: 2, Profile: repoProfile(t)}
	res, _ := Run(context.Background(), cfg, task.Task{ID: "t"}, fakeImpl{}, 1, 1)
	return res, impls
}

func typeName(i implement.Implementer) string {
	if _, ok := i.(implement.Replay); ok {
		return "replay"
	}
	return "original"
}

func applied(r *run) { r.res.StageApply = result.Pass }
func dbDied(r *run) {
	applied(r)
	r.infraError(&r.res.StageGoTest, "go_test", errString("connection reset by peer"))
}
func allPass(r *run) {
	for _, st := range []*string{&r.res.StageApply, &r.res.StageBuild, &r.res.StageVet, &r.res.StageLint, &r.res.StageGoTest,
		&r.res.StageHiddenTests, &r.res.StageStackUp, &r.res.StageSeed, &r.res.StageLoadTest, &r.res.StagePerf} {
		*st = result.Pass
	}
}

func candidateFails(r *run) {
	applied(r)
	r.fail(&r.res.StageHiddenTests, "hidden_tests", errString("1/9 hidden tests did not pass"))
}

type errString string

func (e errString) Error() string { return string(e) }

func TestRun_RetriesGradingAfterInfraErrorWithoutRerunningImplementer(t *testing.T) {
	res, impls := stagedRun(t, []func(*run){dbDied, allPass})
	if len(impls) != 2 || impls[0] != "claude:test|original" || impls[1] != "claude:test|replay" {
		t.Fatalf("want the original implementer once, then a replay, got %v", impls)
	}
	if !res.Pass || res.InfraRetries != 1 || !strings.Contains(res.InfraRetryErrors, "connection reset") {
		t.Fatalf("want a pass after 1 retry recording the error, got pass=%v retries=%d errors=%q", res.Pass, res.InfraRetries, res.InfraRetryErrors)
	}
	if res.Implementer != "claude:test" || res.Model != "m" {
		t.Fatalf("retry must keep the original implementer's identity, got %q %q", res.Implementer, res.Model)
	}
}

func TestRun_StopsRetryingAfterLimit(t *testing.T) {
	res, impls := stagedRun(t, []func(*run){dbDied, dbDied, dbDied})
	if len(impls) != 3 || !res.HasInfraError() || res.InfraRetries != 2 {
		t.Fatalf("want 3 tries ending in an infra error with 2 retries, got %d tries, retries=%d, infra=%v", len(impls), res.InfraRetries, res.HasInfraError())
	}
}

func TestRun_NeverRetriesAVerdict(t *testing.T) {
	res, impls := stagedRun(t, []func(*run){candidateFails})
	if len(impls) != 1 || res.Pass || res.InfraRetries != 0 {
		t.Fatalf("a candidate failure is a verdict, got %d tries, retries=%d", len(impls), res.InfraRetries)
	}
}

func TestRun_RerunsTheAgentAfterAnAPIError(t *testing.T) {
	apiDown := func(r *run) {
		r.info.APIError = true
		r.infraError(&r.res.StageApply, "apply", errString("agent session ended on an API error: ENOTFOUND"))
	}
	res, impls := stagedRun(t, []func(*run){apiDown, allPass})
	if len(impls) != 2 || impls[1] != "claude:test|original" {
		t.Fatalf("want the agent run again (not a replay) after an API error, got %v", impls)
	}
	if !res.Pass || res.InfraRetries != 1 || !strings.Contains(res.InfraRetryErrors, "ENOTFOUND") {
		t.Fatalf("want a pass after 1 retry recording the API error, got pass=%v retries=%d errors=%q", res.Pass, res.InfraRetries, res.InfraRetryErrors)
	}
}

func TestRun_NeverRetriesWhenTheImplementerItselfFailed(t *testing.T) {
	agentCrashed := func(r *run) { r.infraError(&r.res.StageApply, "apply", errString("agent exited without a result")) }
	if _, impls := stagedRun(t, []func(*run){agentCrashed}); len(impls) != 1 {
		t.Fatalf("an infra error before the candidate exists must not be retried (it would rerun the agent), got %d tries", len(impls))
	}
}

func TestJudgePerf(t *testing.T) {
	newRun := func() *run {
		return &run{cfg: Config{P95Tolerance: 0.25, P95SlackMs: 5, ErrorRateTolerance: 0.001}, res: result.New()}
	}
	steady := []loadSummary{{P95Ms: 100, Requests: 2000, Checks: 1}, {P95Ms: 102, Requests: 2100, Checks: 1}, {P95Ms: 98, Requests: 2050, Checks: 1}}
	slower := []loadSummary{{P95Ms: 140, Requests: 1900, Checks: 1}, {P95Ms: 150, Requests: 1800, Checks: 1}, {P95Ms: 145, Requests: 1850, Checks: 1}}

	if problems, ok := newRun().judgePerf([][]loadSummary{steady, steady}); !ok || len(problems) != 0 {
		t.Fatalf("same numbers: want no problems, got %v %v", problems, ok)
	}
	r := newRun()
	if problems, ok := r.judgePerf([][]loadSummary{steady, slower}); !ok || len(problems) != 1 || !strings.Contains(problems[0], "p95") {
		t.Fatalf("+45%% p95: want one p95 problem, got %v %v", problems, ok)
	}
	if r.res.StagePerf != result.Skipped {
		t.Fatal("judgePerf must leave the verdict to its caller, which confirms a failure first")
	}
	stalled := []loadSummary{{P95Ms: 100, Requests: 2000, Checks: 1}, {P95Ms: 30000, Requests: 20, Checks: 1}, {P95Ms: 98, Requests: 2050, Checks: 1}}
	r = newRun()
	if _, ok := r.judgePerf([][]loadSummary{stalled, steady}); ok || r.res.StagePerf != result.Error {
		t.Fatalf("a starved baseline round is an infra error, got ok=%v stage=%s", ok, r.res.StagePerf)
	}
}

func TestPassed_RequiresEveryStageTheProfileDefines(t *testing.T) {
	p := repoProfile(t)
	r := &run{cfg: Config{Profile: p}, res: result.New()}
	allPass(r)
	if !r.passed() {
		t.Fatal("every stage passed but the run did not")
	}
	r.res.StageVet = result.Fail
	if r.passed() {
		t.Fatal("a failed vet stage must fail the run")
	}

	// A profile without vet, seed or a stack requires none of them.
	bare := *p
	bare.Stages = []profile.Stage{{Name: profile.StageTest, Run: "go test -json ./...", Results: profile.ReaderGoJSON}}
	bare.App, bare.Seed, bare.Load = nil, nil, nil
	r = &run{cfg: Config{Profile: &bare}, res: result.New()}
	r.res.StageApply, r.res.StageGoTest, r.res.StageHiddenTests = result.Pass, result.Pass, result.Pass
	if !r.passed() {
		t.Fatalf("stages the profile doesn't define must not be required: %+v", r.res)
	}
}

func TestFail_HostFailureIsAnInfraError(t *testing.T) {
	r := &run{res: result.New()}
	r.fail(&r.res.StageBuild, "build", errString("compile: writing output: write $WORK/b286/_pkg_.a: no space left on device"))
	if r.res.StageBuild != result.Error || !r.res.HasInfraError() {
		t.Fatalf("a full disk must be an infra error, got %s", r.res.StageBuild)
	}
	r = &run{res: result.New()}
	r.fail(&r.res.StageBuild, "build", errString("undefined: foo"))
	if r.res.StageBuild != result.Fail {
		t.Fatalf("a compile error is the candidate's, got %s", r.res.StageBuild)
	}
}

func TestRepoFor_NeedsAWebRepoForWebTasks(t *testing.T) {
	c := Config{APIRepo: "/api"}
	if got, err := c.RepoFor(task.RepoAPI); err != nil || got != "/api" {
		t.Fatalf("api task: %q, %v", got, err)
	}
	if _, err := c.RepoFor(task.RepoWeb); err == nil {
		t.Fatal("a web task without -web-repo must be refused")
	}
	c.WebRepo = "/web"
	if got, err := c.RepoFor(task.RepoWeb); err != nil || got != "/web" {
		t.Fatalf("web task: %q, %v", got, err)
	}
}
