package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/luizeduardocarvalho/fixlab/internal/result"
	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

func TestParseGoTest_CountsAndFailures(t *testing.T) {
	out := []byte(`{"Action":"run","Package":"p","Test":"TestA"}
{"Action":"pass","Package":"p","Test":"TestA/sub"}
{"Action":"pass","Package":"p","Test":"TestA"}
{"Action":"fail","Package":"p","Test":"TestB"}
{"Action":"skip","Package":"p","Test":"TestC"}
{"Action":"fail","Package":"p"}
not json
`)
	s := parseGoTest(out)
	if s.total != 4 || s.passed != 2 || s.failed != 1 || s.skipped != 1 {
		t.Fatalf("counts: got total=%d passed=%d failed=%d skipped=%d", s.total, s.passed, s.failed, s.skipped)
	}
	if s.topLevel["TestA"] != "pass" || s.topLevel["TestB"] != "fail" || s.topLevel["TestC"] != "skip" {
		t.Fatalf("topLevel: %v", s.topLevel)
	}
	if _, ok := s.topLevel["TestA/sub"]; ok {
		t.Fatal("subtests must not be recorded as top-level")
	}
	if len(s.failedNames) != 2 || s.failedNames[0] != "TestB" || s.failedNames[1] != "package p" {
		t.Fatalf("failedNames: %v", s.failedNames)
	}
}

func TestTestInfraFailure(t *testing.T) {
	markers := []string{"failed to start postgres container", "connection reset by peer"}
	dead := []byte(`{"Action":"output","Test":"TestX","Output":"read tcp 127.0.0.1:1->127.0.0.1:2: read: connection reset by peer\n"}`)
	if got := testInfraFailure(dead, markers); got != "connection reset by peer" {
		t.Fatalf("want marker, got %q", got)
	}
	plain := []byte(`{"Action":"output","Test":"TestX","Output":"want 404, got 200\n"}`)
	if got := testInfraFailure(plain, markers); got != "" {
		t.Fatalf("want no marker for an assertion failure, got %q", got)
	}
}

func TestCombine_MedianP95AndWeightedRates(t *testing.T) {
	got := combine([]loadSummary{
		{P95Ms: 50, ErrorRate: 0, Checks: 1, Requests: 100},
		{P95Ms: 900, ErrorRate: 0.5, Checks: 0.5, Requests: 100}, // one spiky round
		{P95Ms: 55, ErrorRate: 0, Checks: 1, Requests: 200},
	})
	if got.P95Ms != 55 {
		t.Fatalf("want median p95 55, got %v", got.P95Ms)
	}
	if got.Requests != 400 || got.ErrorRate != 0.125 || got.Checks != 0.875 {
		t.Fatalf("weighted rates: got %+v", got)
	}
}

func TestGradeHidden(t *testing.T) {
	names := []string{"TestA", "TestB"}
	cases := []struct {
		name     string
		topLevel map[string]string
		infra    string
		want     string
		passed   int
	}{
		{"all pass", map[string]string{"TestA": "pass", "TestB": "pass"}, "", result.Pass, 2},
		{"one fails", map[string]string{"TestA": "pass", "TestB": "fail"}, "", result.Fail, 1},
		{"not run", map[string]string{"TestA": "pass"}, "", result.Fail, 1},
		// Without Docker the DB tests skip: nothing was graded.
		{"skipped", map[string]string{"TestA": "skip", "TestB": "skip"}, "", result.Error, 0},
		{"skip beats fail", map[string]string{"TestA": "fail", "TestB": "skip"}, "", result.Error, 0},
		{"db died", map[string]string{"TestA": "pass", "TestB": "fail"}, "connection reset by peer", result.Error, 1},
	}
	for _, c := range cases {
		g := gradeHidden(names, c.topLevel, c.infra)
		if g.status != c.want || g.passed != c.passed || g.passed+g.failed != len(names) {
			t.Errorf("%s: got %+v, want %s with %d passed", c.name, g, c.want, c.passed)
		}
	}
	if g := gradeHidden(nil, nil, ""); g.status != result.Error {
		t.Errorf("no hidden tests: got %s", g.status)
	}
}

func TestHiddenCollision(t *testing.T) {
	dir := t.TempDir()
	src := "package x_test\n\ntype fixture struct{}\n\nfunc (fixture) seedItem() {}\n\nfunc seedTeam() {}\n\nvar shared = 1\n\nfunc TestA(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(dir, "hidden_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &run{}
	r.task.Dir = dir
	r.task.HiddenTests = []task.HiddenTest{{From: "hidden_test.go", To: "x/x_test.go"}}
	for out, want := range map[string]string{
		"vet: x/x_test.go:7:6: seedTeam redeclared in this block": "seedTeam",
		"x/mine_test.go:3:5: shared redeclared in this block":     "shared",
		"x/mine_test.go:3:5: fixture redeclared in this block":    "fixture",
		// A method name is not package-level, and a name only the candidate
		// declares twice is the candidate's own mistake.
		"x/a.go:3:5: seedItem redeclared in this block": "",
		"x/a.go:3:5: helper redeclared in this block":   "",
		"x/a.go:3:5: undefined: seedTeam":               "",
	} {
		if got := r.hiddenCollision(out); got != want {
			t.Errorf("%q: got %q, want %q", out, got, want)
		}
	}
}

func TestStalledRound(t *testing.T) {
	// The 9a0191 run on issue 9: baseline round 2 starved while Docker stalled.
	if i, ok := stalledRound([]loadSummary{{Requests: 674}, {Requests: 20}, {Requests: 2560}}); !ok || i != 1 {
		t.Fatalf("want round 2 stalled, got %d %v", i, ok)
	}
	if _, ok := stalledRound([]loadSummary{{Requests: 2400}, {Requests: 2100}, {Requests: 2560}}); ok {
		t.Fatal("normal variation flagged as a stall")
	}
	if _, ok := stalledRound(nil); ok {
		t.Fatal("no rounds flagged as a stall")
	}
}
