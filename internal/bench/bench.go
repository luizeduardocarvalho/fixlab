// Package bench reads a workspace's benchmark.yaml: the tasks a strategy is measured
// on, the default strategy, the rule a challenger must meet to replace it,
// and the log of past changes to the default.
package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Benchmark is <workspace>/benchmark.yaml.
type Benchmark struct {
	Default string   `yaml:"default"` // strategy name
	Runs    int      `yaml:"runs"`    // runs per task for make bench
	Tasks   []string `yaml:"tasks"`   // task.yaml paths, relative to the eval dir
	AdoptIf Rule     `yaml:"adopt_if"`
	History []Change `yaml:"history"`
}

// Rule is what a challenger must meet, on every benchmark task, to replace
// the default.
type Rule struct {
	MinRuns          int     `yaml:"min_runs"`           // graded runs per task, for both strategies
	MaxTokenIncrease float64 `yaml:"max_token_increase"` // 0.10 = 10% more tokens, allowed only if some task's pass rate improves
}

// Change is one decision about the default.
type Change struct {
	Date     string `yaml:"date" json:"date"`
	Default  string `yaml:"default" json:"default"`
	Reason   string `yaml:"reason" json:"reason"`
	Evidence string `yaml:"evidence" json:"evidence"`
}

// Read parses and validates a benchmark file.
func Read(path string) (Benchmark, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Benchmark{}, err
	}
	var b Benchmark
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&b); err != nil {
		return Benchmark{}, fmt.Errorf("parse %s: %w", path, err)
	}
	var problems []string
	if b.Default == "" {
		problems = append(problems, "default is required")
	} else if _, err := os.Stat(filepath.Join(filepath.Dir(path), "strategies", b.Default+".yaml")); err != nil {
		problems = append(problems, fmt.Sprintf("default %q has no strategies/%s.yaml", b.Default, b.Default))
	}
	if len(b.Tasks) == 0 {
		problems = append(problems, "at least one task is required")
	}
	for _, t := range b.Tasks {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), t)); err != nil {
			problems = append(problems, fmt.Sprintf("task %s: %v", t, err))
		}
	}
	if b.Runs < 1 {
		b.Runs = 3
	}
	if b.AdoptIf.MinRuns < 1 {
		b.AdoptIf.MinRuns = 3
	}
	if len(problems) > 0 {
		return Benchmark{}, fmt.Errorf("invalid benchmark %s: %s", path, strings.Join(problems, "; "))
	}
	return b, nil
}
