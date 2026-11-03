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

// WriteTestTools is what a test-writing session may use: read the code and
// write its one test file, plus the project's build and test commands
// (Agent.ProjectTools) to compile and run tests.
var WriteTestTools = []string{"Read", "Edit", "Write", "Glob", "Grep"}

// WriteTests runs the agent on a snapshot of ws.Task.BaseCommit with a
// test-writing prompt and returns the test file it wrote (ws.TestFile). It
// never sees a fix: the snapshot is the unfixed base. A session that changes
// any other file is an error.
func (a Agent) WriteTests(ctx context.Context, ws Workspace) ([]byte, Info, error) {
	info := Info{Kind: "agent", Effort: a.Effort, PromptVariant: variantName(a.PromptPath)}
	if ws.TestFile == "" || filepath.IsAbs(ws.TestFile) || strings.HasPrefix(filepath.Clean(ws.TestFile), "..") || !strings.HasSuffix(ws.TestFile, "_test.go") {
		return nil, info, fmt.Errorf("test file %q must be a repo-relative *_test.go path", ws.TestFile)
	}
	if prefix, _ := TestPrefixes(ws.Task.ID); prefix == "" {
		return nil, info, fmt.Errorf("task id %q has no issue number, so its tests can't get a unique prefix", ws.Task.ID)
	}
	sandbox, err := os.MkdirTemp("", "fixlab-tests-")
	if err != nil {
		return nil, info, infra(err)
	}
	defer os.RemoveAll(sandbox)
	if err := exportSnapshot(ctx, ws.Repo, ws.Task.BaseCommit, sandbox, a.Strip); err != nil {
		return nil, info, infra(fmt.Errorf("export snapshot: %w", err))
	}
	if _, err := os.Stat(filepath.Join(sandbox, ws.TestFile)); err == nil {
		return nil, info, fmt.Errorf("%s already exists at the base commit; pick a new file name", ws.TestFile)
	}
	if info.ClaudeMDVariant, info.ClaudeMDSHA256, err = a.applyClaudeMD(sandbox); err != nil {
		return nil, info, infra(err)
	}
	if err := initSandboxRepo(ctx, sandbox, ws.Task.BaseCommit, nil); err != nil {
		return nil, info, infra(err)
	}
	prompt, err := renderPrompt(a.PromptPath, ws)
	if err != nil {
		return nil, info, infra(err)
	}
	info.PromptSHA256 = sha(prompt)
	if err := os.MkdirAll(ws.Artifacts, 0o755); err != nil {
		return nil, info, infra(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Artifacts, "prompt.md"), []byte(prompt), 0o644); err != nil {
		return nil, info, infra(err)
	}
	if info.CLIVersion, err = a.version(ctx); err != nil {
		return nil, info, infra(err)
	}
	a.tools, a.Plugins = slices.Concat(WriteTestTools, a.ProjectTools), nil
	if err := a.run(ctx, ws, sandbox, prompt, &info); err != nil {
		return nil, info, err
	}
	info.DeniedPathRefs = countDeniedRefs(filepath.Join(ws.Artifacts, "transcript.jsonl"), a.DeniedPaths)

	status, err := git(ctx, sandbox, nil, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, info, infra(err)
	}
	var other []string
	for _, line := range strings.Split(strings.TrimRight(string(status), "\n"), "\n") {
		if line == "" {
			continue
		}
		if path := strings.TrimSpace(line[3:]); filepath.Clean(path) != filepath.Clean(ws.TestFile) {
			other = append(other, path)
		}
	}
	if len(other) > 0 {
		return nil, info, fmt.Errorf("the session changed files other than %s: %s", ws.TestFile, strings.Join(other, ", "))
	}
	src, err := os.ReadFile(filepath.Join(sandbox, ws.TestFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, info, fmt.Errorf("the session did not write %s", ws.TestFile)
	}
	if err != nil {
		return nil, info, infra(err)
	}
	if len(bytes.TrimSpace(src)) == 0 {
		return nil, info, fmt.Errorf("%s is empty", ws.TestFile)
	}
	return src, info, nil
}
