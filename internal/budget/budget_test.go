package budget

import (
	"strings"
	"testing"

	"github.com/luizeduardocarvalho/fixlab/internal/result"
)

type outcome int

const (
	passed outcome = iota
	failed
	infra
)

// attempt is a recorded agent attempt whose inputs are named by version: the
// same version means the same prompt and hidden tests.
func attempt(experiment, version string, o outcome) result.Result {
	r := result.Result{RunID: experiment + "-" + version, ImplementerKind: "agent", Experiment: experiment,
		PromptSHA256: "prompt-" + version, GradingSHA256: "grading-" + version}
	switch o {
	case passed:
		r.Pass, r.StageGoTest = true, result.Pass
	case failed:
		r.StageGoTest = result.Fail
	case infra:
		r.StageBuild = result.Error
	}
	return r
}

// legacy strips the grading hash, as on attempts recorded before it existed.
func legacy(r result.Result) result.Result {
	r.GradingSHA256 = ""
	return r
}

func times(n int, r result.Result) []result.Result {
	out := make([]result.Result, n)
	for i := range out {
		out[i] = r
	}
	return out
}

func join(groups ...[]result.Result) []result.Result {
	var out []result.Result
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func inputs(version string) Inputs {
	return Inputs{Prompt: "prompt-" + version, Grading: "grading-" + version}
}

func TestCheck(t *testing.T) {
	const bench = "refine-then-implement"
	regrade := attempt(bench, "v1", failed)
	regrade.RegradedFrom = "old-run"
	reference := attempt(bench, "v1", failed)
	reference.ImplementerKind = "ref"

	cases := []struct {
		name       string
		prior      []result.Result
		experiment string
		version    string
		runs       int
		refusal    string // substring of the error; "" = allowed
	}{
		{"first benchmark round", nil, bench, "v1", 3, ""},
		{"more than a round at once", nil, bench, "v1", 4, "more than the 3 attempts"},
		{"benchmark rerun at the same version", times(3, attempt(bench, "v1", failed)), bench, "v1", 1, "would bias it"},
		{"benchmark at a new task version", times(3, attempt(bench, "v1", failed)), bench, "v2", 3, ""},
		{"infra errors don't use the budget", join(times(2, attempt(bench, "v1", failed)), times(1, attempt(bench, "v1", infra))), bench, "v1", 1, ""},
		{"regrades don't use the budget", join(times(2, attempt(bench, "v1", failed)), []result.Result{regrade}), bench, "v1", 1, ""},
		{"reference and patch runs don't count", join(times(2, attempt(bench, "v1", failed)), []result.Result{reference}), bench, "v1", 1, ""},
		{"other experiments don't count", times(3, attempt("as-filed", "v1", failed)), bench, "v1", 3, ""},

		{"first search round", nil, ReferenceSearch, "v1", 3, ""},
		{"search again without changing anything", times(3, attempt(ReferenceSearch, "v1", failed)), ReferenceSearch, "v1", 3, "start round 2 only after fixing the spec or tests"},
		{"second round after the spec changed", times(3, attempt(ReferenceSearch, "v1", failed)), ReferenceSearch, "v2", 3, ""},
		{"third round", join(times(3, attempt(ReferenceSearch, "v1", failed)), times(3, attempt(ReferenceSearch, "v2", failed))), ReferenceSearch, "v3", 3, "already ran 2 rounds"},
		{"topping up a round after infra errors", join(times(2, attempt(ReferenceSearch, "v1", failed)), times(1, attempt(ReferenceSearch, "v1", infra))), ReferenceSearch, "v1", 1, ""},
		{"a spec change alone starts a new round", times(3, attempt(ReferenceSearch, "v1", failed)), ReferenceSearch, "spec-only", 3, ""},
		{"attempts recorded before the grading hash don't count", times(3, legacy(attempt(bench, "v1", failed))), bench, "v1", 3, ""},
		{"search after a pass", []result.Result{attempt(ReferenceSearch, "v1", passed)}, ReferenceSearch, "v1", 1, "promote it"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Check(c.prior, c.experiment, inputs(c.version), c.runs)
			switch {
			case c.refusal == "" && err != nil:
				t.Fatalf("want allowed, got %v", err)
			case c.refusal != "" && err == nil:
				t.Fatalf("want refused (%q), got allowed", c.refusal)
			case c.refusal != "" && !strings.Contains(err.Error(), c.refusal):
				t.Fatalf("want an error containing %q, got %v", c.refusal, err)
			}
		})
	}
}
