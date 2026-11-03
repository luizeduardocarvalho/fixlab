package report

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luizeduardocarvalho/fixlab/internal/result"
	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

func TestClassOf_LargestMeasureWins(t *testing.T) {
	cases := []struct {
		s    Size
		want string
	}{
		{Size{ProdLines: 10, ProdFiles: 1, Functions: 1}, "XS"},
		{Size{ProdLines: 10, ProdFiles: 6, Functions: 1}, "L"}, // small diff, wide spread
		{Size{ProdLines: 110, ProdFiles: 1, Functions: 6}, "M"},
		{Size{ProdLines: 5000, ProdFiles: 1, Functions: 1}, "XL"},
	}
	for _, c := range cases {
		if got := classOf(c.s); got != c.want {
			t.Errorf("classOf(%+v) = %s, want %s", c.s, got, c.want)
		}
	}
}

func TestHunkFunc(t *testing.T) {
	for line, want := range map[string]string{
		"@@ -40,2 +40,3 @@ func (h *ItemHandler) CreateItem(w http.ResponseWriter": "CreateItem",
		"@@ -1 +1 @@ func helper() error {":                                        "helper",
		"@@ -10 +10 @@ export function useItems() {":                               "useItems",
		"@@ -10 +10 @@ export const ItemList = () => {":                            "ItemList",
	} {
		m := hunkFunc.FindStringSubmatch(line)
		if m == nil || m[1] != want {
			t.Errorf("%q: got %v, want %s", line, m, want)
		}
	}
	if hunkFunc.MatchString("@@ -1 +1 @@ import (") {
		t.Error("import block matched as a function")
	}
}

// TestBuild covers sizing from a real git diff, current vs old task
// versions, regrade replacement and per-test outcomes.
func TestBuild(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	write(t, filepath.Join(repo, "a.go"), "package a\n\nfunc One() int {\n\treturn 1\n}\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "base")
	base := gitOut(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "a.go"), "package a\n\nfunc One() int {\n\tx := 1\n\treturn x\n}\n")
	write(t, filepath.Join(repo, "a_test.go"), "package a\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "fix")
	ref := gitOut(t, repo, "rev-parse", "HEAD")

	tasks := t.TempDir()
	dir := filepath.Join(tasks, "t1")
	write(t, filepath.Join(dir, "hidden_test.go"), "package a\n\nfunc TestFix(t *testing.T) {}\n")
	write(t, filepath.Join(dir, "task.yaml"), "id: t1\ntask_type: bug_fix\narea: x\nbase_commit: "+base+"\nreference_commit: "+ref+
		"\ndescription: |\n  Fix one thing\n  in a\nhidden_tests:\n  - from: hidden_test.go\n    to: a_test.go\n")
	tk, err := task.Read(filepath.Join(dir, "task.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	hist := t.TempDir()
	attempt := func(id, sha, regradeOf string, pass bool, goTest string) {
		r := result.New()
		r.RunID, r.TaskID, r.TaskSHA256, r.RegradedFrom, r.Pass = id, "t1", sha, regradeOf, pass
		r.ImplementerKind, r.Implementer, r.Experiment, r.StartedAt = "agent", "claude:m", "baseline", "2026-10-23T10:00:0"+id[len(id)-1:]+"Z"
		d := filepath.Join(hist, "attempts", "t1", id)
		b, _ := json.Marshal(r)
		write(t, filepath.Join(d, "result.json"), string(b))
		write(t, filepath.Join(d, "go-test.json"), goTest)
	}
	cur := tk.SHA256()
	attempt("r1", "oldversion0000", "", false, "")
	attempt("r2", cur, "", false, `{"Action":"fail","Test":"TestFix"}`+"\n")
	attempt("r3", cur, "r2", true, `{"Action":"pass","Test":"TestFix/sub"}`+"\n"+`{"Action":"pass","Test":"TestFix"}`+"\n")

	d, err := Build(Input{HistoryDir: hist, TasksDir: tasks, Repos: map[string]string{task.RepoAPI: repo}, Now: time.Unix(0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Warnings) != 0 {
		t.Fatalf("warnings: %v", d.Warnings)
	}
	s := d.Tasks[0].Size
	if s == nil || s.ProdLines != 3 || s.ProdFiles != 1 || s.TestLines != 1 || s.Functions != 1 || s.Class != "XS" || s.IssueWords != 5 || s.HiddenTests != 1 {
		t.Fatalf("size: %+v", s)
	}
	ids := map[string]Attempt{}
	for _, a := range d.Attempts {
		ids[a.RunID] = a
	}
	if _, ok := ids["r2"]; ok || len(ids) != 2 {
		t.Fatalf("want r2 replaced by its regrade, got %v", ids)
	}
	if ids["r1"].Current || !ids["r3"].Current {
		t.Fatalf("current flags wrong: %+v", ids)
	}
	if len(d.Tests) != 1 || d.Tests[0].RunID != "r3" || d.Tests[0].Outcome != "pass" {
		t.Fatalf("tests: %+v", d.Tests)
	}

	out := filepath.Join(t.TempDir(), "report.html")
	if err := Write(out, d); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if strings.Contains(string(b), dataMarker) || !strings.Contains(string(b), `"id":"t1"`) {
		t.Fatal("data not embedded")
	}
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	gitOut(t, dir, args...)
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestTestKinds(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "test-kinds.yaml"), "- kind: security\n  label: Safe\n  tests: [\"*_NoCrossLab*\"]\n- kind: contract\n  tests: [\"*Returns404\", \"*_NoCrossLabX\"]\n")
	tests := []string{"TestA_NoCrossLabX", "TestA_Returns404", "TestControl"}
	kinds, err := testKinds(dir, tests)
	if err != nil {
		t.Fatal(err)
	}
	// A test belongs to the first kind it matches; unmatched tests form "other".
	want := []TestKind{
		{Kind: "security", Label: "Safe", Tests: []string{"TestA_NoCrossLabX"}},
		{Kind: "contract", Label: "contract", Tests: []string{"TestA_Returns404"}},
		{Kind: "other", Label: "Other hidden tests", Tests: []string{"TestControl"}},
	}
	got, _ := json.Marshal(kinds)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Fatalf("got %s, want %s", got, exp)
	}

	write(t, filepath.Join(dir, "test-kinds.yaml"), "- kind: typo\n  tests: [\"*Nope*\"]\n")
	if _, err := testKinds(dir, tests); err == nil || !strings.Contains(err.Error(), "matches no hidden test") {
		t.Fatalf("want error for a kind matching nothing, got %v", err)
	}
	if kinds, err := testKinds(t.TempDir(), tests); err != nil || len(kinds) != 0 {
		t.Fatalf("no file: got %v, %v", kinds, err)
	}
}

func TestLatest_RegradeReplacesOnlyTheSameTaskVersion(t *testing.T) {
	in := []result.Result{
		{RunID: "a", TaskSHA256: "v1"},
		{RunID: "a2", TaskSHA256: "v1", RegradedFrom: "a"}, // same tests: replaces a
		{RunID: "b", TaskSHA256: "v1"},
		{RunID: "b2", TaskSHA256: "v2", RegradedFrom: "b"}, // new tests: both kept
	}
	var ids []string
	for _, r := range Latest(in) {
		ids = append(ids, r.RunID)
	}
	if strings.Join(ids, ",") != "a2,b,b2" {
		t.Fatalf("got %v", ids)
	}
}

func TestSummarizeFailures(t *testing.T) {
	d := Data{
		Tasks: []TaskInfo{{ID: "t", HiddenTests: []string{"TestSec", "TestContract"},
			TestKinds: []TestKind{{Kind: "security", Tests: []string{"TestSec"}}, {Kind: "contract", Tests: []string{"TestContract"}}}}},
		Attempts: []Attempt{
			{RunID: "pass", TaskID: "t", Agent: true, Current: true, Pass: true, Variant: "v"},
			{RunID: "miss", TaskID: "t", Agent: true, Current: true, Variant: "v", FailureStage: "go_test", StartedAt: "2"},
			{RunID: "infra", TaskID: "t", Agent: true, Current: true, Infra: true, InfraRetries: 2, Variant: "v", StartedAt: "3"},
			{RunID: "control", TaskID: "t", Current: true, Variant: "patch:p1.diff"},
			{RunID: "ref", TaskID: "t", Current: true, Variant: "reference", FailureStage: "perf", StartedAt: "1"},
			{RunID: "old", TaskID: "t", Agent: true, Variant: "v"},
		},
		Tests: []TestOutcome{
			{RunID: "pass", Test: "TestSec", Outcome: "pass"}, {RunID: "pass", Test: "TestContract", Outcome: "pass"},
			{RunID: "miss", Test: "TestSec", Outcome: "pass"}, {RunID: "miss", Test: "TestContract", Outcome: "fail"},
			{RunID: "infra", Test: "TestSec", Outcome: "skip"}, {RunID: "infra", Test: "TestContract", Outcome: "skip"},
		},
	}
	summarizeFailures(&d)

	var ids []string
	for _, f := range d.Failures {
		ids = append(ids, f.RunID)
	}
	// Newest first; passes, controls and old-version attempts left out.
	if strings.Join(ids, ",") != "infra,miss,ref" {
		t.Fatalf("failures: got %v", ids)
	}
	if len(d.Failures[0].Missed) != 0 {
		t.Fatalf("an infra error lists no missed tests, got %+v", d.Failures[0].Missed)
	}
	miss := d.Failures[1]
	if len(miss.Missed) != 1 || miss.Missed[0].Test != "TestContract" || miss.Missed[0].Kind != "contract" || miss.Dir != "attempts/t/miss" {
		t.Fatalf("missed: got %+v", miss)
	}
	if len(d.Hardest) != 1 || d.Hardest[0].Attempts != 2 {
		t.Fatalf("hardest counts agent attempts without infra errors: got %+v", d.Hardest)
	}
	if top := d.Hardest[0].Tests[0]; top.Test != "TestContract" || top.Failed != 1 || d.Hardest[0].Tests[1].Failed != 0 {
		t.Fatalf("hardest order: got %+v", d.Hardest[0].Tests)
	}
}

func TestConfigLabels_SameConfigIsOneVariant(t *testing.T) {
	agent := func(id, exp, strategy, prompt, started string) result.Result {
		return result.Result{RunID: id, ImplementerKind: "agent", Model: "opus", Experiment: exp, Strategy: strategy, PromptSHA256: prompt, StartedAt: started}
	}
	rs := []result.Result{
		agent("a", "trimmed-v2-refined-issue", "", "p1", "1"),
		agent("b", "refine-then-implement", "refine-then-implement", "p1", "2"), // same prompt: same variant
		agent("c", "baseline", "", "p0", "3"),
		{RunID: "r", Implementer: "ref:abc", StartedAt: "4"},
	}
	labels := configLabels(rs)
	if configKey(rs[0]) != configKey(rs[1]) || labels[configKey(rs[0])] != "refine-then-implement" {
		t.Fatalf("same config must share the strategy's name, got %q and %q", labels[configKey(rs[0])], labels[configKey(rs[1])])
	}
	if labels[configKey(rs[2])] != "baseline" || labels[configKey(rs[3])] != "reference" {
		t.Fatalf("got %v", labels)
	}
}
