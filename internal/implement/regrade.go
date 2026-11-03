package implement

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Regrade replays a recorded attempt's diff, so an improved grader (new hidden
// tests, harness fixes) can score past attempts without re-running, or
// re-paying for, the agent. The original attempt's Info is carried over.
type Regrade struct {
	AttemptDir string // <history>/attempts/<task>/<run_id>
}

type recordedAttempt struct {
	RunID           string   `json:"run_id"`
	TaskID          string   `json:"task_id"`
	Implementer     string   `json:"implementer"`
	ImplementerKind string   `json:"implementer_kind"`
	Model           string   `json:"model"`
	Effort          string   `json:"effort"`
	AgentCLIVersion string   `json:"agent_cli_version"`
	PromptVariant   string   `json:"prompt_variant"`
	PromptSHA256    string   `json:"prompt_sha256"`
	ClaudeMDVariant string   `json:"claude_md_variant"`
	ClaudeMDSHA256  string   `json:"claude_md_sha256"`
	AgentOutcome    string   `json:"agent_outcome"`
	AgentCostUSD    *float64 `json:"agent_cost_usd"`
	InputTokens     *int     `json:"agent_input_tokens"`
	OutputTokens    *int     `json:"agent_output_tokens"`
	CacheRead       *int     `json:"agent_cache_read_tokens"`
	CacheWrite      *int     `json:"agent_cache_write_tokens"`
	Turns           *int     `json:"agent_turns"`
	DurationSec     *float64 `json:"agent_duration_sec"`
	DeniedPathRefs  *int     `json:"agent_denied_path_refs"`
	Plugins         string   `json:"agent_plugins"`
	PluginsSHA256   string   `json:"agent_plugins_sha256"`
	SkillsUsed      string   `json:"agent_skills_used"`
	Setup           string   `json:"agent_setup"`
}

func (g Regrade) load() (recordedAttempt, error) {
	var a recordedAttempt
	b, err := os.ReadFile(filepath.Join(g.AttemptDir, "result.json"))
	if err != nil {
		return a, err
	}
	return a, json.Unmarshal(b, &a)
}

// Replay applies a diff an implementer already produced and reports that
// implementer's Info, so grading can be retried without running it again.
type Replay struct {
	DiffPath string
	Info     Info
	As       string // the original implementer's name
}

func (p Replay) Name() string { return p.As }

func (p Replay) Implement(ctx context.Context, ws Workspace) (Info, error) {
	diff, err := os.ReadFile(p.DiffPath)
	if err != nil {
		return p.Info, infra(fmt.Errorf("replay: %w", err))
	}
	return p.Info, applyDiff(ctx, ws, diff)
}

// Name is the original implementer's, so regraded attempts group with it.
func (g Regrade) Name() string {
	a, err := g.load()
	if err != nil || a.Implementer == "" {
		return "regrade:" + filepath.Base(g.AttemptDir)
	}
	return a.Implementer
}

// Source returns the run_id and task_id of the attempt being regraded.
func (g Regrade) Source() (runID, taskID string, err error) {
	a, err := g.load()
	return a.RunID, a.TaskID, err
}

func (g Regrade) Implement(ctx context.Context, ws Workspace) (Info, error) {
	a, err := g.load()
	if err != nil {
		return Info{}, infra(fmt.Errorf("regrade: %w", err))
	}
	info := Info{
		Kind: a.ImplementerKind, Model: a.Model, Effort: a.Effort, CLIVersion: a.AgentCLIVersion,
		PromptVariant: a.PromptVariant, PromptSHA256: a.PromptSHA256,
		ClaudeMDVariant: a.ClaudeMDVariant, ClaudeMDSHA256: a.ClaudeMDSHA256, Outcome: a.AgentOutcome,
		CostUSD: deref(a.AgentCostUSD), InputTokens: derefInt(a.InputTokens), OutputTokens: derefInt(a.OutputTokens),
		CacheReadTokens: derefInt(a.CacheRead), CacheWriteTokens: derefInt(a.CacheWrite), Turns: derefInt(a.Turns),
		DurationSec: deref(a.DurationSec), DeniedPathRefs: derefInt(a.DeniedPathRefs),
		Plugins: a.Plugins, PluginsSHA256: a.PluginsSHA256, SkillsUsed: a.SkillsUsed, Setup: a.Setup,
	}
	diff, err := os.ReadFile(filepath.Join(g.AttemptDir, "candidate.diff"))
	if err != nil {
		return info, infra(fmt.Errorf("regrade: %w", err))
	}
	// Keep the original transcript and prompt with the regraded attempt.
	for _, f := range []string{"transcript.jsonl", "prompt.md"} {
		if b, err := os.ReadFile(filepath.Join(g.AttemptDir, f)); err == nil {
			_ = os.MkdirAll(ws.Artifacts, 0o755)
			_ = os.WriteFile(filepath.Join(ws.Artifacts, f), b, 0o644)
		}
	}
	return info, applyDiff(ctx, ws, diff)
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
