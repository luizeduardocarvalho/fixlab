// Package result defines the flat, per-run JSON record the harness emits.
//
// Every field is always present so rows from different tasks and runs can be
// loaded into one table (e.g. a heat map of area × task_type by pass rate).
// Perf metrics are null when the load stage did not run.
package result

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Stage statuses.
const (
	Pass    = "pass"
	Fail    = "fail"
	Skipped = "skipped"
	Error   = "error" // infrastructure problem, not a verdict on the candidate
)

// SchemaVersion is bumped whenever a field is added, removed or redefined.
const SchemaVersion = 4

type Result struct {
	SchemaVersion int     `json:"schema_version"`
	TaskID        string  `json:"task_id"`
	Run           int     `json:"run"`
	Runs          int     `json:"runs"`
	RunID         string  `json:"run_id"`
	Area          string  `json:"area"`
	TaskType      string  `json:"task_type"`
	Repo          string  `json:"repo"`
	Implementer   string  `json:"implementer"`
	Experiment    string  `json:"experiment"`
	BaseCommit    string  `json:"base_commit"`
	StartedAt     string  `json:"started_at"`
	DurationSec   float64 `json:"duration_sec"`

	Pass         bool   `json:"pass"`
	FailureStage string `json:"failure_stage"`
	Error        string `json:"error"`

	StageApply        string `json:"stage_apply"`
	StageBuild        string `json:"stage_build"`
	StageVet          string `json:"stage_vet"`
	StageLint         string `json:"stage_lint"`
	StageTypecheck    string `json:"stage_typecheck"`
	StageGoTest       string `json:"stage_go_test"`
	StageHiddenTests  string `json:"stage_hidden_tests"`
	StageFrontendTest string `json:"stage_frontend_test"`
	StageStackUp      string `json:"stage_stack_up"`
	StageSeed         string `json:"stage_seed"`
	StageLoadTest     string `json:"stage_load_test"`
	StagePerf         string `json:"stage_perf"`

	GoTestsTotal   int `json:"go_tests_total"`
	GoTestsPassed  int `json:"go_tests_passed"`
	GoTestsFailed  int `json:"go_tests_failed"`
	GoTestsSkipped int `json:"go_tests_skipped"`

	HiddenTestsTotal  int `json:"hidden_tests_total"`
	HiddenTestsPassed int `json:"hidden_tests_passed"`
	HiddenTestsFailed int `json:"hidden_tests_failed"`

	FrontendTestsTotal  int `json:"frontend_tests_total"`
	FrontendTestsPassed int `json:"frontend_tests_passed"`
	FrontendTestsFailed int `json:"frontend_tests_failed"`

	LintNewFindings    int `json:"lint_new_findings"`
	TypecheckNewErrors int `json:"typecheck_new_errors"`

	BaselineP95Ms      *float64 `json:"baseline_p95_ms"`
	CandidateP95Ms     *float64 `json:"candidate_p95_ms"`
	P95DeltaPct        *float64 `json:"p95_delta_pct"`
	BaselineErrorRate  *float64 `json:"baseline_error_rate"`
	CandidateErrorRate *float64 `json:"candidate_error_rate"`
	BaselineRequests   int      `json:"baseline_requests"`
	CandidateRequests  int      `json:"candidate_requests"`
	// Candidate change size, measured after apply and before hidden tests.
	// files_outside_reference counts changed files the reference fix didn't
	// touch (null when the task has no reference_commit).
	DiffFiles                 int  `json:"diff_files"`
	DiffLinesAdded            int  `json:"diff_lines_added"`
	DiffLinesRemoved          int  `json:"diff_lines_removed"`
	DiffFilesOutsideReference *int `json:"diff_files_outside_reference"`

	// What produced the candidate. Agent fields are empty/null for ref and patch.
	ImplementerKind     string   `json:"implementer_kind"`
	Model               string   `json:"model"`
	Effort              string   `json:"effort"`
	AgentCLIVersion     string   `json:"agent_cli_version"`
	PromptVariant       string   `json:"prompt_variant"`
	PromptSHA256        string   `json:"prompt_sha256"`
	ClaudeMDVariant     string   `json:"claude_md_variant"`
	ClaudeMDSHA256      string   `json:"claude_md_sha256"`
	AgentOutcome        string   `json:"agent_outcome"`
	AgentCostUSD        *float64 `json:"agent_cost_usd"`
	AgentInputTokens    *int     `json:"agent_input_tokens"`
	AgentOutputTokens   *int     `json:"agent_output_tokens"`
	AgentCacheReadTok   *int     `json:"agent_cache_read_tokens"`
	AgentCacheWriteTok  *int     `json:"agent_cache_write_tokens"`
	AgentTurns          *int     `json:"agent_turns"`
	AgentDurationSec    *float64 `json:"agent_duration_sec"`
	AgentDeniedPathRefs *int     `json:"agent_denied_path_refs"`
	AgentPlugins        string   `json:"agent_plugins"`        // name@version, comma-separated; "" = none
	AgentPluginsSHA256  string   `json:"agent_plugins_sha256"` // hash of the plugins' files
	AgentSkillsUsed     string   `json:"agent_skills_used"`    // skills invoked, comma-separated
	AgentSetup          string   `json:"agent_setup"`          // command run in the snapshot before the agent

	// Provenance: lets a change in the grader be told apart from a change in
	// the agent.
	HarnessVersion string `json:"harness_version"`
	TaskSHA256     string `json:"task_sha256"`
	// Base commit and hidden tests only (task.Task.GradingSHA256); with the
	// prompt hash it keys the attempt cap.
	GradingSHA256 string `json:"grading_sha256"`
	// run_id of the attempt whose recorded diff was replayed ("" if none).
	// Agent fields then describe that original attempt.
	RegradedFrom string `json:"regraded_from"`

	// Share of k6 requests that returned the status the script expected.
	// Catches 4xx regressions that the 5xx rate cannot see.
	BaselineChecksRate  *float64 `json:"baseline_checks_rate"`
	CandidateChecksRate *float64 `json:"candidate_checks_rate"`

	// Grading retried after an infra error (the candidate's diff replayed, the
	// implementer not run again), and the errors that caused each retry.
	InfraRetries     int    `json:"infra_retries"`
	InfraRetryErrors string `json:"infra_retry_errors"`
	// 1 when a failing perf verdict was re-checked with a second load test.
	PerfReruns int `json:"perf_reruns"`

	// The strategy file the agent ran with ("" when set by individual flags).
	Strategy       string `json:"strategy"`
	StrategySHA256 string `json:"strategy_sha256"`

	// Why this attempt ran past the attempt cap (see internal/budget); "" when
	// it was within it.
	CapOverride string `json:"cap_override"`

	// The project profile the run was graded with ("" for web tasks, which
	// have none yet), and the hash of its files.
	Profile       string `json:"profile"`
	ProfileSHA256 string `json:"profile_sha256"`
}

// New returns a Result with every stage marked skipped.
func New() *Result {
	return &Result{
		SchemaVersion:     SchemaVersion,
		StageApply:        Skipped,
		StageBuild:        Skipped,
		StageVet:          Skipped,
		StageLint:         Skipped,
		StageTypecheck:    Skipped,
		StageGoTest:       Skipped,
		StageHiddenTests:  Skipped,
		StageFrontendTest: Skipped,
		StageStackUp:      Skipped,
		StageSeed:         Skipped,
		StageLoadTest:     Skipped,
		StagePerf:         Skipped,
	}
}

// HasInfraError reports whether the run did not reach a verdict because some
// stage hit an infrastructure error and no stage failed on the candidate.
func (r *Result) HasInfraError() bool {
	stages := []string{r.StageApply, r.StageBuild, r.StageVet, r.StageLint, r.StageTypecheck, r.StageGoTest,
		r.StageHiddenTests, r.StageFrontendTest, r.StageStackUp, r.StageSeed, r.StageLoadTest, r.StagePerf}
	infra := false
	for _, s := range stages {
		switch s {
		case Fail:
			return false
		case Error:
			infra = true
		}
	}
	return infra
}

// Write stores the result as indented JSON at path, creating parent dirs.
func (r *Result) Write(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
