package report

import (
	"fmt"
	"strings"
	"testing"
)

// benchData builds one task with a security test and agent attempts for
// two variants: each spec is (variant, pass, security passed, tokens).
func benchData(specs ...struct {
	variant  string
	pass     bool
	security bool
	tokens   int
}) Data {
	d := Data{Tasks: []TaskInfo{{ID: "t", HiddenTests: []string{"TestSec", "TestC"},
		TestKinds: []TestKind{{Kind: "security", Tests: []string{"TestSec"}}, {Kind: "contract", Tests: []string{"TestC"}}}}}}
	for i, s := range specs {
		id := fmt.Sprintf("r%d", i)
		tok, zero := s.tokens, 0
		d.Attempts = append(d.Attempts, Attempt{RunID: id, TaskID: "t", Agent: true, Current: true, Variant: s.variant, Pass: s.pass,
			HiddenPassed: map[bool]int{true: 2, false: 1}[s.pass], HiddenTotal: 2, InputTok: &tok, OutputTok: &zero, CacheReadTok: &zero, CacheWrite: &zero})
		sec := map[bool]string{true: "pass", false: "fail"}[s.security]
		d.Tests = append(d.Tests, TestOutcome{RunID: id, Test: "TestSec", Outcome: sec}, TestOutcome{RunID: id, Test: "TestC", Outcome: map[bool]string{true: "pass", false: "fail"}[s.pass]})
	}
	return d
}

type spec = struct {
	variant  string
	pass     bool
	security bool
	tokens   int
}

func runs(variant string, n int, pass, security bool, tokens int) []spec {
	out := make([]spec, n)
	for i := range out {
		out[i] = spec{variant, pass, security, tokens}
	}
	return out
}

func TestCompare(t *testing.T) {
	rule := CompareRule{MinRuns: 3, MaxTokenIncrease: 0.10}
	cases := []struct {
		name    string
		specs   []spec
		verdict string
		reason  string
	}{
		{"same results, same tokens", append(runs("def", 3, true, true, 100), runs("new", 3, true, true, 105)...), "adopt", ""},
		{"lower pass rate", append(runs("def", 3, true, true, 100), append(runs("new", 2, true, true, 100), runs("new", 1, false, true, 100)...)...), "keep-default", "pass rate"},
		{"security regressed", append(runs("def", 3, false, true, 100), runs("new", 3, false, false, 100)...), "keep-default", "security"},
		{"more tokens, no gain", append(runs("def", 3, true, true, 100), runs("new", 3, true, true, 200)...), "keep-default", "tokens"},
		{"more tokens, but passes more", append(runs("def", 3, false, true, 100), runs("new", 3, true, true, 200)...), "adopt", ""},
		{"too few runs", append(runs("def", 3, true, true, 100), runs("new", 2, true, true, 100)...), "not-enough-data", "needed"},
	}
	for _, c := range cases {
		got := Compare(benchData(c.specs...), "new", "def", []string{"t"}, rule)
		if got.Verdict != c.verdict || (c.reason != "" && !strings.Contains(strings.Join(got.Reasons, "; "), c.reason)) {
			t.Errorf("%s: got %s %v, want %s mentioning %q", c.name, got.Verdict, got.Reasons, c.verdict, c.reason)
		}
	}
}
