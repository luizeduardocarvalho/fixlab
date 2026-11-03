// Package task loads and validates eval task files.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Repo identifiers a task can target.
const (
	RepoAPI = "api"
	RepoWeb = "web"
)

var (
	validTypes = map[string]bool{"bug_fix": true, "feature": true, "refactor": true, "migration": true}
	idPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// HiddenTest is a test file the candidate never sees. From is relative to the
// task file's directory; To is relative to the repo root of the worktree.
type HiddenTest struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// Load overrides the project profile's load settings for this task, field by
// field; zero values keep the profile's.
type Load struct {
	Script   string `yaml:"script"`   // k6 script, relative to the profile dir
	VUs      int    `yaml:"vus"`      // concurrent virtual users
	Duration string `yaml:"duration"` // k6 duration of one round, e.g. "10s"
	Rounds   int    `yaml:"rounds"`   // interleaved rounds per stack; perf compares medians
}

// Task is one eval task file.
type Task struct {
	ID              string       `yaml:"id"`
	Description     string       `yaml:"description"`
	BaseCommit      string       `yaml:"base_commit"`
	TaskType        string       `yaml:"task_type"`
	Area            string       `yaml:"area"`
	Repo            string       `yaml:"repo"`
	HiddenTests     []HiddenTest `yaml:"hidden_tests"`
	ReferenceCommit string       `yaml:"reference_commit"`
	Load            Load         `yaml:"load"`

	// Dir is the absolute directory containing the task file.
	Dir string `yaml:"-"`
	// Refined is <Dir>/refined.md if present: the issue rewritten with the
	// maintainer's decisions. Prompts use it as {{.Refined}}. Like
	// test-kinds.yaml it is not part of the task version: it changes the
	// prompt (whose hash each attempt records), not the grading.
	Refined string `yaml:"-"`
}

// Read parses and validates a task file.
func Read(path string) (Task, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Task{}, err
	}
	var t Task
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return Task{}, fmt.Errorf("parse %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Task{}, err
	}
	t.Dir = filepath.Dir(abs)
	if b, err := os.ReadFile(filepath.Join(t.Dir, "refined.md")); err == nil {
		t.Refined = string(b)
	}

	if t.Repo == "" {
		t.Repo = RepoAPI
	}
	return t, t.validate()
}

func (t Task) validate() error {
	var problems []string
	if !idPattern.MatchString(t.ID) {
		problems = append(problems, "id must match "+idPattern.String())
	}
	if strings.TrimSpace(t.Description) == "" {
		problems = append(problems, "description is required")
	}
	if t.BaseCommit == "" {
		problems = append(problems, "base_commit is required")
	}
	if !validTypes[t.TaskType] {
		problems = append(problems, "task_type must be one of bug_fix, feature, refactor, migration")
	}
	if t.Area == "" {
		problems = append(problems, "area is required")
	}
	if t.Repo != RepoAPI && t.Repo != RepoWeb {
		problems = append(problems, "repo must be api or web")
	}
	if t.Load.Rounds < 0 || t.Load.VUs < 0 {
		problems = append(problems, "load.rounds and load.vus cannot be negative")
	}
	if s := t.Load.Script; s != "" && (filepath.IsAbs(s) || strings.HasPrefix(filepath.Clean(s), "..")) {
		problems = append(problems, "load.script must be a path inside the profile dir")
	}
	if len(t.HiddenTests) == 0 {
		problems = append(problems, "at least one hidden_tests entry is required")
	}
	for _, h := range t.HiddenTests {
		if _, err := os.Stat(filepath.Join(t.Dir, h.From)); err != nil {
			problems = append(problems, fmt.Sprintf("hidden test %q: %v", h.From, err))
		}
		if h.To == "" || filepath.IsAbs(h.To) || strings.HasPrefix(filepath.Clean(h.To), "..") {
			problems = append(problems, fmt.Sprintf("hidden test %q: to must be a relative path inside the repo", h.From))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid task %s: %s", t.ID, strings.Join(problems, "; "))
	}
	return nil
}

var testFuncPattern = regexp.MustCompile(`(?m)^func (Test\w+)\(\w+ \*testing\.T\)`)

// HiddenGoTests lists the top-level Go test functions declared in the task's
// hidden test files.
func (t Task) HiddenGoTests() ([]string, error) {
	var names []string
	for _, h := range t.HiddenTests {
		if !strings.HasSuffix(h.To, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(t.Dir, h.From))
		if err != nil {
			return nil, err
		}
		for _, m := range testFuncPattern.FindAllSubmatch(src, -1) {
			names = append(names, string(m[1]))
		}
	}
	return names, nil
}

// GradingSHA256 hashes what decides an attempt's verdict: the base commit and
// the hidden tests. Unlike SHA256 it leaves out the rest of task.yaml, such
// as reference_commit, which can move without changing what an attempt faces.
func (t Task) GradingSHA256() string {
	h := sha256.New()
	fmt.Fprintf(h, "base:%s\n", t.BaseCommit)
	for _, ht := range t.HiddenTests {
		b, err := os.ReadFile(filepath.Join(t.Dir, ht.From))
		if err != nil {
			fmt.Fprintf(h, "missing:%s\n", ht.From)
			continue
		}
		fmt.Fprintf(h, "%s:%d\n", ht.From, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SHA256 hashes the task file and its hidden tests: a changed oracle must not
// be mistaken for a changed agent.
func (t Task) SHA256() string {
	h := sha256.New()
	files := []string{"task.yaml"}
	for _, ht := range t.HiddenTests {
		files = append(files, ht.From)
	}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(t.Dir, f))
		if err != nil {
			fmt.Fprintf(h, "missing:%s\n", f)
			continue
		}
		fmt.Fprintf(h, "%s:%d\n", f, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}
