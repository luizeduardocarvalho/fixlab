package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/luizeduardocarvalho/fixlab/internal/result"
)

func pnpm(dir, log string, timeout time.Duration, args ...string) cmdSpec {
	return cmdSpec{dir: dir, log: log, timeout: timeout, name: "pnpm", args: args, env: []string{"CI=true"}}
}

// webStages evaluates a repo: web candidate, which has no profile yet: a
// pnpm + Vite app, through install, build, lint, typecheck (as a delta
// against baseline) and Vitest.
func (r *run) webStages(ctx context.Context) {
	for _, wt := range []struct{ dir, log string }{{r.baseWT, "pnpm-install-baseline"}, {r.candWT, "pnpm-install-candidate"}} {
		if _, err := r.exec(ctx, pnpm(wt.dir, wt.log, 10*time.Minute, "install", "--frozen-lockfile", "--prefer-offline")); err != nil {
			if wt.dir == r.baseWT {
				r.infraError(&r.res.StageBuild, "build", err)
			} else {
				r.fail(&r.res.StageBuild, "build", err)
			}
			return
		}
	}

	if _, err := r.exec(ctx, pnpm(r.candWT, "vite-build", 10*time.Minute, "exec", "vite", "build")); err != nil {
		r.fail(&r.res.StageBuild, "build", err)
		return
	}
	r.res.StageBuild = result.Pass

	// eslint exits non-zero on errors only; warnings don't fail the stage.
	if _, err := r.exec(ctx, pnpm(r.candWT, "eslint", 10*time.Minute, "lint")); err != nil {
		r.fail(&r.res.StageLint, "lint", err)
	} else {
		r.res.StageLint = result.Pass
	}

	r.webTypecheck(ctx)
	r.vitest(ctx)
}

var tsErrorPattern = regexp.MustCompile(`(?m)error TS\d+`)

// webTypecheck fails on type errors the candidate introduced. The app
// tsconfig may already have errors on the base, so absolute pass/fail is
// meaningless.
func (r *run) webTypecheck(ctx context.Context) {
	count := func(dir, log string) int {
		out, _ := r.exec(ctx, pnpm(dir, log, 10*time.Minute, "exec", "tsc", "--noEmit", "-p", "tsconfig.app.json"))
		return len(tsErrorPattern.FindAll(out, -1))
	}
	base := count(r.baseWT, "tsc-baseline")
	cand := count(r.candWT, "tsc-candidate")
	r.res.TypecheckNewErrors = max(cand-base, 0)
	if cand > base {
		r.fail(&r.res.StageTypecheck, "typecheck", fmt.Errorf("%d new TypeScript error(s) (baseline %d, candidate %d)", cand-base, base, cand))
		return
	}
	r.res.StageTypecheck = result.Pass
}

type vitestReport struct {
	NumTotalTests  int `json:"numTotalTests"`
	NumPassedTests int `json:"numPassedTests"`
	NumFailedTests int `json:"numFailedTests"`
	TestResults    []struct {
		Name             string `json:"name"`
		AssertionResults []struct {
			Status string `json:"status"`
		} `json:"assertionResults"`
	} `json:"testResults"`
}

func (r *run) vitest(ctx context.Context) {
	report := filepath.Join(r.artifacts, "vitest.json")
	_, runErr := r.exec(ctx, pnpm(r.candWT, "vitest", 15*time.Minute, "exec", "vitest", "run", "--reporter=json", "--outputFile="+report))
	raw, err := os.ReadFile(report)
	if err != nil {
		r.fail(&r.res.StageFrontendTest, "frontend_test", fmt.Errorf("no vitest report: %v (%v)", err, runErr))
		return
	}
	var rep vitestReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		r.infraError(&r.res.StageFrontendTest, "frontend_test", err)
		return
	}
	r.res.FrontendTestsTotal, r.res.FrontendTestsPassed, r.res.FrontendTestsFailed = rep.NumTotalTests, rep.NumPassedTests, rep.NumFailedTests
	if runErr != nil {
		r.fail(&r.res.StageFrontendTest, "frontend_test", runErr)
	} else {
		r.res.StageFrontendTest = result.Pass
	}

	// A hidden test file passes when it ran and every assertion in it passed.
	var failed []string
	for _, h := range r.task.HiddenTests {
		r.res.HiddenTestsTotal++
		ok := false
		for _, tr := range rep.TestResults {
			if !strings.HasSuffix(filepath.ToSlash(tr.Name), filepath.ToSlash(h.To)) {
				continue
			}
			ok = len(tr.AssertionResults) > 0
			for _, a := range tr.AssertionResults {
				ok = ok && a.Status == "passed"
			}
		}
		if ok {
			r.res.HiddenTestsPassed++
		} else {
			r.res.HiddenTestsFailed++
			failed = append(failed, h.To)
		}
	}
	if len(failed) > 0 {
		r.fail(&r.res.StageHiddenTests, "hidden_tests", fmt.Errorf("hidden test files did not pass: %v", failed))
		return
	}
	r.res.StageHiddenTests = result.Pass
}
