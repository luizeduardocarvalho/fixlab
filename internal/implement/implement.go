// Package implement defines how a candidate change gets into a worktree.
//
// The runner only depends on the Implementer interface: Ref and Patch apply
// a known change, Agent has Claude Code write one.
package implement

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

// Workspace is what an Implementer works on: a worktree checked out at
// Task.BaseCommit, created from Repo.
type Workspace struct {
	Task      task.Task
	Repo      string // source repository the worktree was created from
	Worktree  string // absolute path of the candidate worktree
	Artifacts string // dir for implementer outputs (prompt, transcript, …); kept with the result
	GoBin     string // go binary the harness uses; agents get its dir on PATH

	// Task-building sessions only.
	TestFile  string   // the file a test-writing agent must write (repo-relative)
	Avoid     []string // mistakes earlier controls already made, for a control-writing agent
	Draft     string   // the refine draft, for a finalise session
	Decisions string   // the maintainer's decisions, for a finalise session
	Tests     string   // the hidden tests' source, for a test review
	RedCheck  string   // how each hidden test did on the base, for a test review
}

// Info describes what an implementer did. Fields that don't apply are zero.
type Info struct {
	Kind string // "ref", "patch" or "agent"

	// Agent runs only.
	Model            string  // model id reported by the agent CLI
	Effort           string  // requested effort level, "" = CLI default
	CLIVersion       string  // agent CLI version
	PromptVariant    string  // prompt template name
	PromptSHA256     string  // hash of the rendered prompt
	ClaudeMDVariant  string  // "repo", "none" or the replacement file's name
	ClaudeMDSHA256   string  // hash of the CLAUDE.md the agent saw ("" if none)
	CostUSD          float64 // as reported by the CLI
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	Turns            int
	DurationSec      float64
	Outcome          string // CLI result subtype, e.g. "success", "error_max_turns"
	SessionID        string
	DeniedPathRefs   int    // mentions of denied dirs in the transcript; should be 0
	Plugins          string // loaded plugins as name@version, comma-separated ("" = none)
	PluginsSHA256    string // hash of the loaded plugins' files
	SkillsUsed       string // skills the agent invoked, comma-separated in first-use order
	Setup            string // command run in the snapshot before the agent started ("" = none)
	Answer           string // the session's final message (used by refine, not recorded in results)
	APIError         bool   // the session ended on a model API error (terminal_reason "api_error")
}

// InfraError marks an implementer failure that says nothing about the
// candidate (the agent CLI is missing, can't authenticate, crashed, …).
type InfraError struct{ Err error }

func (e InfraError) Error() string { return e.Err.Error() }
func (e InfraError) Unwrap() error { return e.Err }

func infra(err error) error {
	if err == nil {
		return nil
	}
	return InfraError{err}
}

// Implementer applies a candidate change to a workspace.
type Implementer interface {
	// Name identifies the implementer in results, e.g. "ref:ba5ab6a".
	Name() string
	Implement(ctx context.Context, ws Workspace) (Info, error)
}

// Ref applies the diff between the task's base commit and a git ref from the
// source repo. The ref should descend from the base commit; otherwise the diff
// also carries unrelated changes.
type Ref struct {
	Ref string
}

func (r Ref) Name() string { return "ref:" + r.Ref }

func (r Ref) Implement(ctx context.Context, ws Workspace) (Info, error) {
	info := Info{Kind: "ref"}
	diff, err := git(ctx, ws.Repo, nil, "diff", "--binary", ws.Task.BaseCommit, r.Ref)
	if err != nil {
		return info, err
	}
	if len(bytes.TrimSpace(diff)) == 0 {
		return info, fmt.Errorf("ref %s has no changes relative to %s", r.Ref, ws.Task.BaseCommit)
	}
	return info, applyDiff(ctx, ws, diff)
}

// Patch applies a unified diff file (git diff format) to the worktree.
type Patch struct {
	Path string
}

func (p Patch) Name() string { return "patch:" + filepath.Base(p.Path) }

func (p Patch) Implement(ctx context.Context, ws Workspace) (Info, error) {
	info := Info{Kind: "patch"}
	diff, err := os.ReadFile(p.Path)
	if err != nil {
		return info, err
	}
	return info, applyDiff(ctx, ws, diff)
}

// applyDiff applies a git diff to the candidate worktree and keeps a copy as
// candidate.diff next to the result, so every attempt's change is on record.
func applyDiff(ctx context.Context, ws Workspace, diff []byte) error {
	if ws.Artifacts != "" {
		if err := os.MkdirAll(ws.Artifacts, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(ws.Artifacts, "candidate.diff"), diff, 0o644); err != nil {
			return err
		}
	}
	_, err := git(ctx, ws.Worktree, diff, "apply", "--binary", "--whitespace=nowarn", "-")
	return err
}

func git(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %v: %w: %s", args, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}
