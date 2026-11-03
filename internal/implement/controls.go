package implement

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ControlTools is what a control-writing session may use besides the
// project's commands (Agent.ProjectTools): WriteTestTools, plus git diff and
// log to see the reference fix it starts from.
var ControlTools = append(append([]string{}, WriteTestTools...), "Bash(git diff:*)", "Bash(git log:*)")

// WriteControl runs the agent on the task's reference fix, without the
// hidden tests, and asks for one plausible mistake: a wrong fix the hidden
// tests should catch. It returns the control as a diff from the base commit
// (the reference fix plus the mistake) and the agent's description of it.
func (a Agent) WriteControl(ctx context.Context, ws Workspace) ([]byte, string, Info, error) {
	info := Info{Kind: "agent", Effort: a.Effort, PromptVariant: variantName(a.PromptPath)}
	t := ws.Task
	if t.ReferenceCommit == "" {
		return nil, "", info, fmt.Errorf("task %s has no reference_commit to start a control from", t.ID)
	}
	// The reference fix without the hidden tests: the agent must not tune its
	// mistake to slip past them.
	args := []string{"diff", "--binary", t.BaseCommit, t.ReferenceCommit, "--", "."}
	for _, h := range t.HiddenTests {
		args = append(args, ":(exclude)"+h.To)
	}
	fix, err := git(ctx, ws.Repo, nil, args...)
	if err != nil {
		return nil, "", info, infra(err)
	}
	if len(bytes.TrimSpace(fix)) == 0 {
		return nil, "", info, fmt.Errorf("reference %s changes nothing but the hidden tests", t.ReferenceCommit)
	}

	sandbox, err := os.MkdirTemp("", "fixlab-control-")
	if err != nil {
		return nil, "", info, infra(err)
	}
	defer os.RemoveAll(sandbox)
	if err := exportSnapshot(ctx, ws.Repo, t.BaseCommit, sandbox, a.Strip); err != nil {
		return nil, "", info, infra(fmt.Errorf("export snapshot: %w", err))
	}
	if info.ClaudeMDVariant, info.ClaudeMDSHA256, err = a.applyClaudeMD(sandbox); err != nil {
		return nil, "", info, infra(err)
	}
	if err := initSandboxRepo(ctx, sandbox, t.BaseCommit, nil); err != nil {
		return nil, "", info, infra(err)
	}
	if _, err := git(ctx, sandbox, fix, "apply", "--index", "--binary", "--whitespace=nowarn", "-"); err != nil {
		return nil, "", info, infra(fmt.Errorf("apply the reference fix: %w", err))
	}
	if _, err := git(ctx, sandbox, nil, "-c", "user.name=eval", "-c", "user.email=eval@example.test",
		"commit", "-q", "--no-verify", "-m", "reference fix"); err != nil {
		return nil, "", info, infra(err)
	}

	prompt, err := renderPrompt(a.PromptPath, ws)
	if err != nil {
		return nil, "", info, infra(err)
	}
	info.PromptSHA256 = sha(prompt)
	if err := os.MkdirAll(ws.Artifacts, 0o755); err != nil {
		return nil, "", info, infra(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Artifacts, "prompt.md"), []byte(prompt), 0o644); err != nil {
		return nil, "", info, infra(err)
	}
	if info.CLIVersion, err = a.version(ctx); err != nil {
		return nil, "", info, infra(err)
	}
	a.tools, a.Plugins = slices.Concat(ControlTools, a.ProjectTools), nil
	if err := a.run(ctx, ws, sandbox, prompt, &info); err != nil {
		return nil, "", info, err
	}
	info.DeniedPathRefs = countDeniedRefs(filepath.Join(ws.Artifacts, "transcript.jsonl"), a.DeniedPaths)

	if _, err := git(ctx, sandbox, nil, "add", "-A"); err != nil {
		return nil, "", info, infra(err)
	}
	changed, err := git(ctx, sandbox, nil, "diff", "--cached", "--name-only", "HEAD")
	if err != nil {
		return nil, "", info, infra(err)
	}
	files := strings.Fields(string(changed))
	if len(files) == 0 {
		return nil, "", info, errors.New("the session left the reference fix unchanged")
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			return nil, "", info, fmt.Errorf("the session changed test file %s; a control changes only code", f)
		}
	}
	if strings.TrimSpace(info.Answer) == "" {
		return nil, "", info, errors.New("the session ended without describing its mistake")
	}
	diff, err := sandboxDiff(ctx, sandbox)
	if err != nil {
		return nil, "", info, infra(err)
	}
	return diff, info.Answer, info, nil
}
