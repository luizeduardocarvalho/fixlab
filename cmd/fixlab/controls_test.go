package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseControlAnswer(t *testing.T) {
	slug, mistake := parseControlAnswer("Done.\n\n```\nslug: Skip Purchase_List sync\nmistake: the purchase-list sync keeps the old rule.\n```\n")
	if slug != "skip-purchase-list-sync" || mistake != "the purchase-list sync keeps the old rule." {
		t.Errorf("got %q, %q", slug, mistake)
	}
	if slug, mistake := parseControlAnswer("I changed the boundary"); slug != "control" || mistake != "I changed the boundary" {
		t.Errorf("fallback got %q, %q", slug, mistake)
	}
}

func TestReadControls_HeaderOrFileName(t *testing.T) {
	dir := t.TempDir()
	for name, src := range map[string]string{
		"p1-incomplete.diff": "diff --git a/x b/x\n",
		"p2-boundary.diff":   "# mistake: uses < for <=\n# fails: A (1 of 3 hidden tests)\ndiff --git a/x b/x\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readControls(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].mistake != "p1-incomplete" || got[1].mistake != "uses < for <=" {
		t.Errorf("got %+v", got)
	}
}

func TestTrimPreamble(t *testing.T) {
	if got := trimPreamble("I've finished reading the code.\n\n> **Refined spec.**\n\n## Summary\nx"); got != "> **Refined spec.**\n\n## Summary\nx" {
		t.Errorf("got %q", got)
	}
	if got := trimPreamble("no headings at all"); got != "no headings at all" {
		t.Errorf("got %q", got)
	}
}
