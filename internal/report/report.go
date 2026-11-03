// Package report turns the recorded eval history into one self-contained
// HTML page: pass rates by task size and variant, per-test outcomes, cost,
// latency and diff size for every attempt.
package report

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/luizeduardocarvalho/fixlab/internal/bench"
	"github.com/luizeduardocarvalho/fixlab/internal/implement"
	"github.com/luizeduardocarvalho/fixlab/internal/result"
	"github.com/luizeduardocarvalho/fixlab/internal/strategy"
	"github.com/luizeduardocarvalho/fixlab/internal/task"
	"gopkg.in/yaml.v3"
)

//go:embed assets/report.html
var page string

const dataMarker = "__REPORT_DATA__"

// Input says where the history, the task definitions and the repos are.
type Input struct {
	HistoryDir     string
	TasksDir       string            // <workspace>/tasks
	Repos          map[string]string // task.RepoAPI / task.RepoWeb → checkout
	HarnessVersion string
	Now            time.Time
	Benchmark      *bench.Benchmark    // optional: adds the strategy comparisons
	Strategies     []strategy.Strategy // named configurations; attempts matching one count as it
	Workspace      string              // resolves strategy paths
}

// BenchmarkInfo is the benchmark as the page shows it.
type BenchmarkInfo struct {
	Default string         `json:"default"`
	Tasks   []string       `json:"tasks"` // task ids
	MinRuns int            `json:"min_runs"`
	MaxTok  float64        `json:"max_token_increase"`
	History []bench.Change `json:"history"`
}

// Data is everything the page draws. It is embedded in the page as JSON.
type Data struct {
	GeneratedAt    string         `json:"generated_at"`
	HarnessVersion string         `json:"harness_version"`
	Classes        []ClassInfo    `json:"classes"`
	Tasks          []TaskInfo     `json:"tasks"`
	Attempts       []Attempt      `json:"attempts"`
	Tests          []TestOutcome  `json:"tests"`
	Failures       []Failure      `json:"failures"`
	Benchmark      *BenchmarkInfo `json:"benchmark"`
	Comparisons    []Comparison   `json:"comparisons"`
	Hardest        []TaskHardest  `json:"hardest"`
	Warnings       []string       `json:"warnings"`
}

// Failure is an attempt on the current tests that didn't fully pass: an
// agent attempt or a reference fix (controls are meant to fail and are left
// out).
type Failure struct {
	RunID        string       `json:"run_id"`
	TaskID       string       `json:"task_id"`
	Variant      string       `json:"variant"`
	Strategy     string       `json:"strategy"`
	Agent        bool         `json:"agent"`
	StartedAt    string       `json:"started_at"`
	Stage        string       `json:"stage"`
	Infra        bool         `json:"infra"`
	InfraRetries int          `json:"infra_retries"`
	HiddenPassed int          `json:"hidden_passed"`
	HiddenTotal  int          `json:"hidden_total"`
	Missed       []MissedTest `json:"missed"` // hidden tests that didn't pass
	Error        string       `json:"error"`  // first line of the run's error
	Dir          string       `json:"dir"`    // attempts/<task>/<run>, in the history repo
}

// MissedTest is a hidden test an attempt didn't pass.
type MissedTest struct {
	Test    string `json:"test"`
	Kind    string `json:"kind"`
	Outcome string `json:"outcome"` // fail, skip or missing
}

// TaskHardest ranks a task's hidden tests by how often agent attempts on the
// current tests failed them.
type TaskHardest struct {
	TaskID   string       `json:"task_id"`
	Attempts int          `json:"attempts"` // agent attempts with per-test results, infra errors excluded
	Tests    []TestRecord `json:"tests"`
}

// TestRecord is one hidden test's record across those attempts.
type TestRecord struct {
	Test   string `json:"test"`
	Kind   string `json:"kind"`
	Failed int    `json:"failed"`
}

// ClassInfo is one size class as the page explains it.
type ClassInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	MaxLines    int    `json:"max_lines"`
}

// TaskInfo is a task definition found under TasksDir.
type TaskInfo struct {
	ID          string     `json:"id"`
	Repo        string     `json:"repo"`
	TaskType    string     `json:"task_type"`
	Area        string     `json:"area"`
	Version     string     `json:"version"` // short task sha; attempts graded by other versions are not "current"
	Title       string     `json:"title"`   // first line of the description
	Size        *Size      `json:"size"`
	HiddenTests []string   `json:"hidden_tests"`
	TestKinds   []TestKind `json:"test_kinds"`
}

// TestKind is a group of hidden tests that check one property, e.g.
// "security" or "contract", read from the task's test-kinds.yaml.
type TestKind struct {
	Kind  string   `json:"kind"`
	Label string   `json:"label"`
	Tests []string `json:"tests"`
}

// Attempt is one recorded run, trimmed to what the page shows.
type Attempt struct {
	RunID        string   `json:"run_id"`
	TaskID       string   `json:"task_id"`
	StartedAt    string   `json:"started_at"`
	Variant      string   `json:"variant"`
	Agent        bool     `json:"agent"`
	Implementer  string   `json:"implementer"`
	Model        string   `json:"model"`
	Effort       string   `json:"effort"`
	Prompt       string   `json:"prompt"`
	ClaudeMD     string   `json:"claude_md"`
	Plugins      string   `json:"plugins"`
	SkillsUsed   string   `json:"skills_used"`
	Strategy     string   `json:"strategy"`
	Experiment   string   `json:"experiment"` // the label the run was recorded under
	Config       string   `json:"config"`     // what actually ran; attempts with the same config are one variant
	InfraRetries int      `json:"infra_retries"`
	Error        string   `json:"error"`
	TaskVersion  string   `json:"task_version"`
	Current      bool     `json:"current"`
	RegradedFrom string   `json:"regraded_from"`
	Pass         bool     `json:"pass"`
	Infra        bool     `json:"infra"`
	FailureStage string   `json:"failure_stage"`
	HiddenPassed int      `json:"hidden_passed"`
	HiddenTotal  int      `json:"hidden_total"`
	CostUSD      *float64 `json:"cost_usd"` // list price of the tokens, as Claude Code reports it; not what a subscription bills
	InputTok     *int     `json:"input_tokens"`
	OutputTok    *int     `json:"output_tokens"`
	CacheReadTok *int     `json:"cache_read_tokens"`
	CacheWrite   *int     `json:"cache_write_tokens"`
	AgentSec     *float64 `json:"agent_sec"`
	Turns        *int     `json:"turns"`
	DiffLines    int      `json:"diff_lines"`
	DiffFiles    int      `json:"diff_files"`
	OutsideRef   *int     `json:"outside_ref"`
	DeniedRefs   *int     `json:"denied_refs"`
	P95BaseMs    *float64 `json:"p95_base_ms"`
	P95CandMs    *float64 `json:"p95_cand_ms"`
	P95DeltaPct  *float64 `json:"p95_delta_pct"`
	ErrorRate    *float64 `json:"error_rate"`
	Harness      string   `json:"harness"`
}

// TestOutcome is one hidden test's result in one current attempt.
type TestOutcome struct {
	RunID   string `json:"run_id"`
	Test    string `json:"test"`
	Outcome string `json:"outcome"` // pass, fail, skip or missing
}

// Build collects the page data.
func Build(in Input) (Data, error) {
	d := Data{
		GeneratedAt:    in.Now.UTC().Format(time.RFC3339),
		HarnessVersion: in.HarnessVersion,
		Classes:        make([]ClassInfo, 0, len(Classes)),
		Tasks:          []TaskInfo{},
		Attempts:       []Attempt{},
		Tests:          []TestOutcome{},
		Failures:       []Failure{},
		Comparisons:    []Comparison{},
		Hardest:        []TaskHardest{},
		Warnings:       []string{},
	}
	for _, c := range Classes {
		d.Classes = append(d.Classes, ClassInfo{Name: c.Name, Description: c.Description, MaxLines: c.MaxLines})
	}

	tasks := map[string]TaskInfo{}
	files, err := filepath.Glob(filepath.Join(in.TasksDir, "*", "task.yaml"))
	if err != nil {
		return d, err
	}
	for _, f := range files {
		t, err := task.Read(f)
		if err != nil {
			d.Warnings = append(d.Warnings, err.Error())
			continue
		}
		info := TaskInfo{ID: t.ID, Repo: t.Repo, TaskType: t.TaskType, Area: t.Area, Version: short(t.SHA256()),
			Title: strings.TrimSpace(strings.SplitN(strings.TrimSpace(t.Description), "\n", 2)[0])}
		if info.HiddenTests, err = t.HiddenGoTests(); err != nil {
			d.Warnings = append(d.Warnings, fmt.Sprintf("%s: hidden tests: %v", t.ID, err))
		}
		if info.TestKinds, err = testKinds(t.Dir, info.HiddenTests); err != nil {
			d.Warnings = append(d.Warnings, fmt.Sprintf("%s: test kinds: %v", t.ID, err))
		}
		if size, err := Measure(t, in.Repos[t.Repo]); err != nil {
			d.Warnings = append(d.Warnings, fmt.Sprintf("%s: size: %v", t.ID, err))
		} else {
			info.Size = &size
		}
		tasks[t.ID] = info
		d.Tasks = append(d.Tasks, info)
	}
	sort.Slice(d.Tasks, func(i, j int) bool { return d.Tasks[i].ID < d.Tasks[j].ID })

	results, err := LoadAttempts(in.HistoryDir, "")
	if err != nil {
		return d, err
	}
	latest := Latest(results)
	labels := configLabels(latest)
	// An attempt whose configuration is exactly what a strategy would run on
	// its task counts as that strategy, whatever label it was recorded under.
	for key, name := range strategyKeys(in, tasks) {
		labels[key] = name
	}
	for _, r := range latest {
		t, known := tasks[r.TaskID]
		a := Attempt{
			RunID: r.RunID, TaskID: r.TaskID, StartedAt: r.StartedAt, Variant: labels[configKey(r)],
			Experiment: r.Experiment, Config: configKey(r),
			Agent: r.ImplementerKind == "agent", Implementer: r.Implementer, Model: r.Model, Effort: r.Effort,
			Prompt: r.PromptVariant, ClaudeMD: r.ClaudeMDVariant, Plugins: r.AgentPlugins, SkillsUsed: r.AgentSkillsUsed, TaskVersion: short(r.TaskSHA256),
			Current: known && short(r.TaskSHA256) == t.Version, RegradedFrom: r.RegradedFrom,
			Pass: r.Pass, Infra: r.HasInfraError(), FailureStage: r.FailureStage,
			HiddenPassed: r.HiddenTestsPassed, HiddenTotal: r.HiddenTestsTotal,
			CostUSD: r.AgentCostUSD, InputTok: r.AgentInputTokens, OutputTok: r.AgentOutputTokens,
			CacheReadTok: r.AgentCacheReadTok, CacheWrite: r.AgentCacheWriteTok, AgentSec: r.AgentDurationSec, Turns: r.AgentTurns,
			DiffLines: r.DiffLinesAdded + r.DiffLinesRemoved, DiffFiles: r.DiffFiles,
			OutsideRef: r.DiffFilesOutsideReference, DeniedRefs: r.AgentDeniedPathRefs,
			P95BaseMs: r.BaselineP95Ms, P95CandMs: r.CandidateP95Ms, P95DeltaPct: r.P95DeltaPct,
			ErrorRate: r.CandidateErrorRate, Harness: r.HarnessVersion,
			Strategy: r.Strategy, InfraRetries: r.InfraRetries, Error: firstLine(r.Error, 240),
		}
		d.Attempts = append(d.Attempts, a)
		if !a.Current || len(t.HiddenTests) == 0 {
			continue
		}
		outcomes, err := topLevelOutcomes(filepath.Join(in.HistoryDir, "attempts", r.TaskID, r.RunID, "go-test.json"))
		if err != nil {
			d.Warnings = append(d.Warnings, fmt.Sprintf("%s: %v", r.RunID, err))
			continue
		}
		for _, name := range t.HiddenTests {
			o := outcomes[name]
			if o == "" {
				o = "missing"
			}
			d.Tests = append(d.Tests, TestOutcome{RunID: r.RunID, Test: name, Outcome: o})
		}
	}
	summarizeFailures(&d)
	if b := in.Benchmark; b != nil {
		info := &BenchmarkInfo{Default: b.Default, MinRuns: b.AdoptIf.MinRuns, MaxTok: b.AdoptIf.MaxTokenIncrease, History: b.History, Tasks: []string{}}
		for _, p := range b.Tasks {
			if t, err := task.Read(filepath.Join(filepath.Dir(in.TasksDir), p)); err == nil {
				info.Tasks = append(info.Tasks, t.ID)
			}
		}
		d.Benchmark = info
		for _, s := range in.Strategies {
			if s.Name != b.Default {
				d.Comparisons = append(d.Comparisons, Compare(d, s.Name, b.Default, info.Tasks,
					CompareRule{MinRuns: b.AdoptIf.MinRuns, MaxTokenIncrease: b.AdoptIf.MaxTokenIncrease}))
			}
		}
	}
	return d, nil
}

// summarizeFailures fills d.Failures and d.Hardest from the attempts and
// per-test outcomes already in d.
func summarizeFailures(d *Data) {
	outcomes := map[string]map[string]string{}
	for _, o := range d.Tests {
		if outcomes[o.RunID] == nil {
			outcomes[o.RunID] = map[string]string{}
		}
		outcomes[o.RunID][o.Test] = o.Outcome
	}
	tasks := map[string]TaskInfo{}
	for _, t := range d.Tasks {
		tasks[t.ID] = t
	}
	kindOf := func(t TaskInfo, test string) string {
		for _, k := range t.TestKinds {
			if slices.Contains(k.Tests, test) {
				return k.Kind
			}
		}
		return ""
	}
	hardest := map[string]*TaskHardest{}
	for _, a := range d.Attempts {
		t, ok := tasks[a.TaskID]
		if !ok || !a.Current {
			continue
		}
		control := !a.Agent && a.Variant != "reference"
		got := outcomes[a.RunID]
		if a.Agent && !a.Infra && got != nil {
			h := hardest[a.TaskID]
			if h == nil {
				h = &TaskHardest{TaskID: a.TaskID}
				for _, name := range t.HiddenTests {
					h.Tests = append(h.Tests, TestRecord{Test: name, Kind: kindOf(t, name)})
				}
				hardest[a.TaskID] = h
			}
			h.Attempts++
			for i := range h.Tests {
				if got[h.Tests[i].Test] != "pass" {
					h.Tests[i].Failed++
				}
			}
		}
		if a.Pass || control {
			continue
		}
		f := Failure{
			RunID: a.RunID, TaskID: a.TaskID, Variant: a.Variant, Strategy: a.Strategy, Agent: a.Agent,
			StartedAt: a.StartedAt, Stage: a.FailureStage, Infra: a.Infra, InfraRetries: a.InfraRetries,
			HiddenPassed: a.HiddenPassed, HiddenTotal: a.HiddenTotal, Error: a.Error,
			Missed: []MissedTest{}, Dir: path.Join("attempts", a.TaskID, a.RunID),
		}
		// An infra error graded nothing: its "missed" tests would be noise.
		if got != nil && !a.Infra {
			for _, name := range t.HiddenTests {
				if o := got[name]; o != "pass" {
					f.Missed = append(f.Missed, MissedTest{Test: name, Kind: kindOf(t, name), Outcome: o})
				}
			}
		}
		d.Failures = append(d.Failures, f)
	}
	sort.SliceStable(d.Failures, func(i, j int) bool { return d.Failures[i].StartedAt > d.Failures[j].StartedAt })
	for _, t := range d.Tasks {
		if h := hardest[t.ID]; h != nil {
			sort.SliceStable(h.Tests, func(i, j int) bool { return h.Tests[i].Failed > h.Tests[j].Failed })
			d.Hardest = append(d.Hardest, *h)
		}
	}
}

func firstLine(s string, max int) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// testKinds assigns each hidden test to the first kind in
// <dir>/test-kinds.yaml whose glob matches its name. Tests no kind matches
// form an "other" kind. Without the file there are no kinds.
func testKinds(dir string, tests []string) ([]TestKind, error) {
	b, err := os.ReadFile(filepath.Join(dir, "test-kinds.yaml"))
	if errors.Is(err, fs.ErrNotExist) {
		return []TestKind{}, nil
	}
	if err != nil {
		return nil, err
	}
	var spec []struct {
		Kind  string   `yaml:"kind"`
		Label string   `yaml:"label"`
		Tests []string `yaml:"tests"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return nil, err
	}
	kinds := make([]TestKind, 0, len(spec)+1)
	assigned := map[string]bool{}
	for _, k := range spec {
		if k.Kind == "" {
			return nil, errors.New("every entry needs a kind")
		}
		tk := TestKind{Kind: k.Kind, Label: cmp.Or(k.Label, k.Kind), Tests: []string{}}
		for _, name := range tests {
			for _, pat := range k.Tests {
				ok, err := path.Match(pat, name)
				if err != nil {
					return nil, fmt.Errorf("kind %s: %w", k.Kind, err)
				}
				if ok && !assigned[name] {
					assigned[name] = true
					tk.Tests = append(tk.Tests, name)
					break
				}
			}
		}
		if len(tk.Tests) == 0 {
			return nil, fmt.Errorf("kind %s matches no hidden test", k.Kind)
		}
		kinds = append(kinds, tk)
	}
	other := TestKind{Kind: "other", Label: "Other hidden tests", Tests: []string{}}
	for _, name := range tests {
		if !assigned[name] {
			other.Tests = append(other.Tests, name)
		}
	}
	if len(other.Tests) > 0 && len(kinds) > 0 {
		kinds = append(kinds, other)
	}
	return kinds, nil
}

// Latest drops every attempt that a later regrade under the same task
// version replaced, so each agent attempt counts once. A regrade of a regrade
// replaces both.
func Latest(results []result.Result) []result.Result {
	superseded := map[string]bool{}
	for _, r := range results {
		if r.RegradedFrom == "" {
			continue
		}
		for _, o := range results {
			if o.RunID == r.RegradedFrom && o.TaskSHA256 == r.TaskSHA256 {
				superseded[o.RunID] = true
			}
		}
	}
	out := make([]result.Result, 0, len(results))
	for _, r := range results {
		if !superseded[r.RunID] {
			out = append(out, r)
		}
	}
	return out
}

// configKey identifies what an agent attempt actually ran: model, effort and
// the hashes of the prompt, CLAUDE.md and plugins it saw, and its setup
// command. Two experiment labels for the same configuration are one variant.
// Other implementers are keyed by their variant name.
func configKey(r result.Result) string {
	if r.ImplementerKind != "agent" {
		return "impl:" + variant(r)
	}
	return keyOf(r.Model, r.Effort, r.PromptSHA256, r.ClaudeMDSHA256, r.AgentPluginsSHA256, r.AgentSetup)
}

func keyOf(fields ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return hex.EncodeToString(sum[:4])
}

// strategyKeys maps the configuration each strategy would run on each known
// task to the strategy's name. The first strategy listed wins a tie.
func strategyKeys(in Input, tasks map[string]TaskInfo) map[string]string {
	out := map[string]string{}
	files, _ := filepath.Glob(filepath.Join(in.TasksDir, "*", "task.yaml"))
	for _, f := range files {
		t, err := task.Read(f)
		if err != nil {
			continue
		}
		if _, ok := tasks[t.ID]; !ok {
			continue
		}
		for _, s := range in.Strategies {
			var plugins []string
			for _, p := range s.Plugins {
				plugins = append(plugins, strategy.Resolve(p, in.Workspace))
			}
			p, c, pl, err := implement.Fingerprint(context.Background(), strategy.Resolve(s.Prompt, in.Workspace),
				strategy.Resolve(s.ClaudeMD, in.Workspace), plugins, in.Repos[t.Repo], t)
			if err != nil {
				continue // e.g. the prompt needs a refined.md this task lacks
			}
			if k := keyOf(s.Model, s.Effort, p, c, pl, s.Setup); out[k] == "" {
				out[k] = s.Name
			}
		}
	}
	return out
}

// configLabels names each configuration: the strategy name if any attempt
// with it ran from a strategy file (the newest one), else the label of its
// first attempt.
func configLabels(results []result.Result) map[string]string {
	labels := map[string]string{}
	fromStrategy := map[string]bool{}
	for _, r := range results { // oldest first
		k := configKey(r)
		switch {
		case r.Strategy != "":
			labels[k], fromStrategy[k] = r.Strategy, true
		case labels[k] == "" && !fromStrategy[k]:
			labels[k] = variant(r)
		}
	}
	return labels
}

// variant names the arm an attempt belongs to: its experiment label, else
// "reference" for the task's own fix, else the implementer.
func variant(r result.Result) string {
	switch {
	case r.Experiment != "":
		return r.Experiment
	case strings.HasPrefix(r.Implementer, "ref:"):
		return "reference"
	default:
		return r.Implementer
	}
}

// Write renders the page with d embedded.
func Write(path string, d Data) error {
	b, err := json.Marshal(d) // escapes <, > and &, so the JSON can't close its <script>
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Replace(page, dataMarker, string(b), 1)), 0o644)
}

// LoadAttempts reads every result.json under <root>/attempts, oldest first,
// optionally only for one task.
func LoadAttempts(root, taskID string) ([]result.Result, error) {
	var rows []result.Result
	err := filepath.WalkDir(filepath.Join(root, "attempts"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "result.json" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var r result.Result
		if err := json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if taskID == "" || r.TaskID == taskID {
			rows = append(rows, r)
		}
		return nil
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].StartedAt < rows[j].StartedAt })
	return rows, err
}

// topLevelOutcomes reads a go test -json log and returns the final action of
// each top-level test.
func topLevelOutcomes(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev struct{ Action, Test string }
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Test == "" || strings.Contains(ev.Test, "/") {
			continue
		}
		switch ev.Action {
		case "pass", "fail", "skip":
			out[ev.Test] = ev.Action
		}
	}
	return out, sc.Err()
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
