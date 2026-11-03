package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIssueDecisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"answers":[
		{"id":"plan-1","issue":"plan","question":"Where do tasks start?","decided":true,"answer":"From the dependency","followedRecommendation":true},
		{"id":"28-1","issue":28,"question":"Status for another lab's id?","decided":true,"answer":"404","followedRecommendation":false,"note":"match equipment"},
		{"id":"29-1","issue":29,"question":"Other issue","decided":true,"answer":"x","followedRecommendation":true}
	]}`)
	got, err := issueDecisions(path, "28")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- plan-1: Where do tasks start?\n  Answer: From the dependency\n", "- 28-1: Status for another lab's id?\n  Answer: 404 (not the draft's recommendation)\n  Maintainer's note: match equipment\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "29-1") {
		t.Errorf("another issue's decision leaked in:\n%s", got)
	}

	if _, err := issueDecisions(path, "42"); err == nil || !strings.Contains(err.Error(), "no decisions for issue 42") {
		t.Errorf("want an error for an issue with no decisions, got %v", err)
	}
	write(`{"answers":[{"id":"28-1","issue":28,"question":"q","decided":false}]}`)
	if _, err := issueDecisions(path, "28"); err == nil || !strings.Contains(err.Error(), "undecided: 28-1") {
		t.Errorf("want an error naming the undecided question, got %v", err)
	}
}

func TestSummariseGoTest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "red-check.json")
	lines := []string{
		`{"Action":"run","Test":"TestIssue9_Control"}`,
		`{"Action":"pass","Test":"TestIssue9_Control"}`,
		`{"Action":"run","Test":"TestIssue9_Create"}`,
		`{"Action":"output","Test":"TestIssue9_Create","Output":"=== RUN   TestIssue9_Create\n"}`,
		`{"Action":"output","Test":"TestIssue9_Create","Output":"2026/10/28 [ITEMS] Update error: boom\n"}`,
		`{"Action":"output","Test":"TestIssue9_Create/sub","Output":"    issue9_test.go:42: want 201, got 500\n"}`,
		`{"Action":"fail","Test":"TestIssue9_Create/sub"}`,
		`{"Action":"fail","Test":"TestIssue9_Create"}`,
		`{"Action":"fail","Package":"p"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := summariseGoTest(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "pass TestIssue9_Control\nfail TestIssue9_Create\n    issue9_test.go:42: want 201, got 500\n    2026/10/28 [ITEMS] Update error: boom\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestTrimPreambleKeepsEditorialNotesSeparable(t *testing.T) {
	answer := "Here is the spec.\n> **Refined spec.** x\n\n## Summary\ns\n\n## Editorial notes\n- chose 404\n"
	spec, notes, ok := strings.Cut(trimPreamble(answer), editorialHeading)
	if !ok || strings.HasPrefix(spec, "Here") || strings.Contains(spec, "chose") || strings.TrimSpace(notes) != "- chose 404" {
		t.Errorf("spec %q, notes %q", spec, notes)
	}
}
