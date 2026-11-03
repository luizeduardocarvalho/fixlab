package bench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRead_RepoBenchmark(t *testing.T) {
	b, err := Read("../../testdata/example/benchmark.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if b.Default != "refine-then-implement" || len(b.Tasks) < 2 || b.AdoptIf.MinRuns != 3 || len(b.History) == 0 {
		t.Fatalf("unexpected benchmark: %+v", b)
	}
	// The page reads the change log by these keys.
	raw, _ := json.Marshal(b.History[0])
	if !strings.Contains(string(raw), `"date":`) || !strings.Contains(string(raw), `"reason":`) {
		t.Fatalf("history JSON keys: %s", raw)
	}
}

func TestRead_RejectsUnknownDefaultAndTasks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "benchmark.yaml")
	if err := os.WriteFile(path, []byte("default: nope\ntasks: [tasks/missing/task.yaml]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Read(path)
	for _, want := range []string{`default "nope"`, "missing"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q in %v", want, err)
		}
	}
}
