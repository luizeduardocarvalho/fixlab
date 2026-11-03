package implement

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/luizeduardocarvalho/fixlab/internal/task"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"
)

// CoreTools is what an implementing agent may use in any project without
// asking; the project's profile adds its build and test commands
// (Agent.ProjectTools). The run is non-interactive (permission mode
// dontAsk), so anything else is denied: no network tools, no git beyond
// inspecting its own changes.
var CoreTools = []string{
	"Read", "Edit", "Write", "Glob", "Grep",
	"Bash(git diff:*)", "Bash(git status:*)",
}

// Agent runs Claude Code headless on a snapshot of base_commit and applies
// the resulting diff to the candidate worktree.
//
// The agent never works in a git worktree of the real repo: a worktree
// shares refs and objects, so the agent could read the reference fix from
// another branch, or hidden tests from the workspace. It gets a standalone
// export of base_commit (tracked files only, Strip removed) with a single
// fresh commit, in a temp dir outside the repo.
type Agent struct {
	CLI          string        // path to the claude binary
	ConfigDir    string        // CLAUDE_CONFIG_DIR for auth; "" inherits the environment
	Model        string        // "" = CLI default
	Effort       string        // "" = CLI default
	PromptPath   string        // prompt template; {{.Description}}, {{.ID}}, {{.Area}}, {{.TaskType}}
	ClaudeMD     string        // "" keeps the repo's CLAUDE.md, "none" removes it, else a file replacing it
	BudgetUSD    float64       // --max-budget-usd; 0 = no limit
	Timeout      time.Duration // whole agent session
	ProjectTools []string      // the project profile's agent.allow: its build and test commands
	AllowedTools []string      // extra tools on top of CoreTools and ProjectTools, e.g. Task
	DeniedPaths  []string      // absolute dirs the agent must not read (the repos, the workspace)
	Strip        []string      // repo-relative paths removed from the snapshot (the workspace, if it is in the repo)
	Plugins      []string      // plugin dirs loaded with --plugin-dir; user-installed plugins never load
	Setup        string        // shell command run in the snapshot before the agent starts, e.g. building a code graph
	Ignore       []string      // paths git ignores in the sandbox (.git/info/exclude): what Setup generates never reaches the diff

	// tools replaces the implementing tool set for sessions with a narrower
	// job (refine, write-tests, controls).
	tools []string
}

// toolNames turns permission rules into the built-in tool names they cover:
// "Bash(go test:*)" and "Bash(git diff:*)" both need the Bash tool.
func toolNames(rules []string) []string {
	var names []string
	for _, rule := range rules {
		name, _, _ := strings.Cut(rule, "(")
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// Tools is the allowed-tools list the session runs with.
func (a Agent) Tools() []string {
	tools := a.tools
	if tools == nil {
		tools = slices.Concat(CoreTools, a.ProjectTools, a.AllowedTools)
	}
	if len(a.Plugins) > 0 {
		// A plugin's skills are only reachable through the Skill tool.
		tools = append(slices.Clone(tools), "Skill")
	}
	return tools
}

func (a Agent) Name() string {
	model := a.Model
	if model == "" {
		model = "default"
	}
	return "claude:" + model
}

func (a Agent) Implement(ctx context.Context, ws Workspace) (Info, error) {
	info := Info{Kind: "agent", Effort: a.Effort, PromptVariant: variantName(a.PromptPath)}

	sandbox, err := os.MkdirTemp("", "fixlab-agent-")
	if err != nil {
		return info, infra(err)
	}
	defer os.RemoveAll(sandbox)

	if err := exportSnapshot(ctx, ws.Repo, ws.Task.BaseCommit, sandbox, a.Strip); err != nil {
		return info, infra(fmt.Errorf("export snapshot: %w", err))
	}
	if info.ClaudeMDVariant, info.ClaudeMDSHA256, err = a.applyClaudeMD(sandbox); err != nil {
		return info, infra(err)
	}
	if err := a.runSetup(ctx, ws, sandbox); err != nil {
		return info, infra(err)
	}
	info.Setup = a.Setup
	if err := initSandboxRepo(ctx, sandbox, ws.Task.BaseCommit, a.Ignore); err != nil {
		return info, infra(err)
	}

	prompt, err := renderPrompt(a.PromptPath, ws)
	if err != nil {
		return info, infra(err)
	}
	info.PromptSHA256 = sha(prompt)
	if err := os.MkdirAll(ws.Artifacts, 0o755); err != nil {
		return info, infra(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Artifacts, "prompt.md"), []byte(prompt), 0o644); err != nil {
		return info, infra(err)
	}

	if info.CLIVersion, err = a.version(ctx); err != nil {
		return info, infra(err)
	}
	var wantPlugins []string
	if wantPlugins, info.Plugins, info.PluginsSHA256, err = describePlugins(a.Plugins); err != nil {
		return info, infra(err)
	}
	runErr := a.run(ctx, ws, sandbox, prompt, &info)
	if runErr == nil {
		runErr = checkPluginsLoaded(filepath.Join(ws.Artifacts, "transcript.jsonl"), wantPlugins)
	}
	info.DeniedPathRefs = countDeniedRefs(filepath.Join(ws.Artifacts, "transcript.jsonl"), a.DeniedPaths)

	// Collect whatever the agent changed, committed or not, even if the
	// session ended badly: a partial change is still worth recording.
	diff, diffErr := sandboxDiff(ctx, sandbox)
	if runErr != nil {
		return info, runErr
	}
	if diffErr != nil {
		return info, infra(diffErr)
	}
	if len(bytes.TrimSpace(diff)) == 0 {
		return info, errors.New("agent made no changes")
	}
	return info, applyDiff(ctx, ws, diff)
}

// ReadOnlyTools is what a read-only session (refine, finalise, review-tests)
// may use: it reads the code and writes nothing.
var ReadOnlyTools = []string{"Read", "Glob", "Grep"}

// ReadOnly runs the agent read-only on a snapshot of ws.Task.BaseCommit and
// returns its final message: a refined issue, a final spec or a test review,
// depending on the prompt. A session that changed any file is an error.
func (a Agent) ReadOnly(ctx context.Context, ws Workspace) (string, Info, error) {
	info := Info{Kind: "agent", Effort: a.Effort, PromptVariant: variantName(a.PromptPath)}
	sandbox, err := os.MkdirTemp("", "fixlab-readonly-")
	if err != nil {
		return "", info, infra(err)
	}
	defer os.RemoveAll(sandbox)
	if err := exportSnapshot(ctx, ws.Repo, ws.Task.BaseCommit, sandbox, a.Strip); err != nil {
		return "", info, infra(fmt.Errorf("export snapshot: %w", err))
	}
	if info.ClaudeMDVariant, info.ClaudeMDSHA256, err = a.applyClaudeMD(sandbox); err != nil {
		return "", info, infra(err)
	}
	if err := initSandboxRepo(ctx, sandbox, ws.Task.BaseCommit, nil); err != nil {
		return "", info, infra(err)
	}
	prompt, err := renderPrompt(a.PromptPath, ws)
	if err != nil {
		return "", info, infra(err)
	}
	info.PromptSHA256 = sha(prompt)
	if err := os.MkdirAll(ws.Artifacts, 0o755); err != nil {
		return "", info, infra(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Artifacts, "prompt.md"), []byte(prompt), 0o644); err != nil {
		return "", info, infra(err)
	}
	if info.CLIVersion, err = a.version(ctx); err != nil {
		return "", info, infra(err)
	}
	a.tools, a.Plugins = ReadOnlyTools, nil
	if err := a.run(ctx, ws, sandbox, prompt, &info); err != nil {
		return "", info, err
	}
	info.DeniedPathRefs = countDeniedRefs(filepath.Join(ws.Artifacts, "transcript.jsonl"), a.DeniedPaths)
	diff, err := sandboxDiff(ctx, sandbox)
	if err != nil {
		return "", info, infra(err)
	}
	if len(bytes.TrimSpace(diff)) > 0 {
		return "", info, errors.New("read-only session changed files; it must only read")
	}
	if strings.TrimSpace(info.Answer) == "" {
		return "", info, errors.New("read-only session ended without an answer")
	}
	return info.Answer, info, nil
}

func (a Agent) version(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, a.CLI, "--version")
	cmd.Env = a.env("")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", a.CLI, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// productionEnv names the variables an agent never inherits: production and
// cloud credentials. The agent's own test code runs with this environment, so
// keeping them out means nothing it writes can reach production.
var productionEnv = []string{
	"DATABASE_URL", "CLERK_", "PULUMI_", "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLIENT_",
	"CLOUDSDK_", "MAILJET_", "VAPID_", "CLOUDFLARE_", "AWS_", "GH_TOKEN", "GITHUB_TOKEN",
}

// noCloudDir is an empty config dir for gcloud and Pulumi, so a session finds
// no stored login even though the user's machine has one.
func noCloudDir() string {
	dir := filepath.Join(os.TempDir(), "fixlab-no-cloud")
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// SafeEnv is the process environment without production or cloud
// credentials, with gcloud and Pulumi pointed at an empty config dir. Every
// process that runs agent-written code uses it: the agent session, and the
// harness's own builds, tests and stack.
func SafeEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !isProductionEnv(name) {
			env = append(env, kv)
		}
	}
	none := noCloudDir()
	return append(env, "CLOUDSDK_CONFIG="+none, "PULUMI_HOME="+none)
}

func (a Agent) env(goBin string) []string {
	env := SafeEnv()
	if a.ConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+a.ConfigDir)
	}
	if goBin != "" && filepath.IsAbs(goBin) {
		env = append(env, "PATH="+filepath.Dir(goBin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return env
}

func isProductionEnv(name string) bool {
	for _, p := range productionEnv {
		if name == p || (strings.HasSuffix(p, "_") && strings.HasPrefix(name, p)) {
			return true
		}
	}
	return false
}

func (a Agent) run(ctx context.Context, ws Workspace, sandbox, prompt string, info *Info) error {
	if a.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Timeout)
		defer cancel()
	}
	tools := a.Tools()
	args := []string{
		"-p", prompt,
		"--output-format", "stream-json", "--verbose",
		"--permission-mode", "dontAsk",
		"--allowedTools", strings.Join(tools, ","),
		// Only describe the tools it may use: every other tool's definition
		// costs ~10k tokens on every request and is denied anyway.
		"--tools", strings.Join(toolNames(tools), ","),
		// Only the repo's own settings: user-level hooks, plugins and
		// permissions would make attempts depend on whose machine ran them.
		"--setting-sources", "project",
		"--strict-mcp-config",
		"--no-session-persistence",
	}
	if denied := deniedReadRules(a.DeniedPaths); len(denied) > 0 {
		args = append(args, "--disallowedTools", strings.Join(denied, ","))
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if a.Effort != "" {
		args = append(args, "--effort", a.Effort)
	}
	if a.BudgetUSD > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%.2f", a.BudgetUSD))
	}
	for _, p := range a.Plugins {
		args = append(args, "--plugin-dir", p)
	}

	transcript, err := os.Create(filepath.Join(ws.Artifacts, "transcript.jsonl"))
	if err != nil {
		return infra(err)
	}
	defer transcript.Close()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, a.CLI, args...)
	cmd.Dir = sandbox
	cmd.Env = a.env(ws.GoBin)
	cmd.Stderr = &stderr
	cmd.WaitDelay = 10 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return infra(err)
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return infra(fmt.Errorf("start agent: %w", err))
	}
	parseErr := parseStream(io.TeeReader(stdout, transcript), info)
	waitErr := cmd.Wait()
	if info.DurationSec == 0 {
		info.DurationSec = time.Since(start).Seconds()
	}
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("agent timed out after %s", a.Timeout)
	case waitErr != nil && info.Outcome == "":
		// No result event: the session never really ran (auth, CLI crash).
		return infra(fmt.Errorf("agent exited without a result: %w: %s", waitErr, lastLine(stderr.String())))
	case parseErr != nil:
		return infra(fmt.Errorf("parse agent output: %w", parseErr))
	case info.APIError:
		// Not a verdict on the task: the model API failed (network, outage).
		return infra(fmt.Errorf("agent session ended on an API error: %s", lastLine(info.Answer)))
	}
	// A session that hit its turn or budget limit still produced a diff;
	// the harness grades it like any other candidate.
	return nil
}

// parseStream reads Claude Code stream-json events, recording the model from
// the init event, skills from Skill tool calls and usage from the final
// result event.
func parseStream(r io.Reader, info *Info) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var skills []string
	defer func() { info.SkillsUsed = strings.Join(skills, ",") }()
	for sc.Scan() {
		var ev struct {
			Type         string  `json:"type"`
			Subtype      string  `json:"subtype"`
			Model        string  `json:"model"`
			SessionID    string  `json:"session_id"`
			TotalCostUSD float64 `json:"total_cost_usd"`
			Result       string  `json:"result"`
			// TerminalReason "api_error" means the model API couldn't be
			// reached or failed; the session never really worked the task.
			TerminalReason string  `json:"terminal_reason"`
			NumTurns       int     `json:"num_turns"`
			DurationMS     float64 `json:"duration_ms"`
			Usage          struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
			Message struct {
				Content []struct {
					Type  string `json:"type"`
					Name  string `json:"name"`
					Input struct {
						Skill string `json:"skill"`
					} `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "system" && ev.Subtype == "init":
			info.Model, info.SessionID = ev.Model, ev.SessionID
		case ev.Type == "assistant":
			for _, c := range ev.Message.Content {
				if c.Type == "tool_use" && c.Name == "Skill" && c.Input.Skill != "" && !slices.Contains(skills, c.Input.Skill) {
					skills = append(skills, c.Input.Skill)
				}
			}
		case ev.Type == "result":
			info.Outcome = ev.Subtype
			info.Answer = ev.Result
			info.APIError = ev.TerminalReason == "api_error"
			info.CostUSD = ev.TotalCostUSD
			info.Turns = ev.NumTurns
			info.DurationSec = ev.DurationMS / 1000
			info.InputTokens = ev.Usage.InputTokens
			info.OutputTokens = ev.Usage.OutputTokens
			info.CacheReadTokens = ev.Usage.CacheReadInputTokens
			info.CacheWriteTokens = ev.Usage.CacheCreationInputTokens
		}
	}
	return sc.Err()
}

// describePlugins reads each plugin dir's manifest (plugin.json or
// .claude-plugin/plugin.json) and hashes its files, so an attempt records
// exactly which plugin code it ran with.
func describePlugins(dirs []string) (names []string, label, hash string, err error) {
	if len(dirs) == 0 {
		return nil, "", "", nil
	}
	h := sha256.New()
	var labels []string
	for _, dir := range dirs {
		var m struct{ Name, Version string }
		var raw []byte
		for _, f := range []string{".claude-plugin/plugin.json", "plugin.json"} {
			if raw, err = os.ReadFile(filepath.Join(dir, f)); err == nil {
				break
			}
		}
		if err != nil {
			return nil, "", "", fmt.Errorf("plugin %s: no plugin.json", dir)
		}
		if err := json.Unmarshal(raw, &m); err != nil || m.Name == "" {
			return nil, "", "", fmt.Errorf("plugin %s: bad manifest: %v", dir, err)
		}
		names = append(names, m.Name)
		labels = append(labels, m.Name+"@"+cmp.Or(m.Version, "unversioned"))
		err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, path)
			fmt.Fprintf(h, "%s/%s:%d\n", m.Name, filepath.ToSlash(rel), len(b))
			h.Write(b)
			return nil
		})
		if err != nil {
			return nil, "", "", fmt.Errorf("plugin %s: %w", dir, err)
		}
	}
	return names, strings.Join(labels, ","), hex.EncodeToString(h.Sum(nil)), nil
}

// checkPluginsLoaded fails when a requested plugin is missing from the
// session's init event: an attempt that silently ran without it would be
// recorded under the wrong variant.
func checkPluginsLoaded(transcript string, want []string) error {
	if len(want) == 0 {
		return nil
	}
	f, err := os.Open(transcript)
	if err != nil {
		return infra(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var ev struct {
			Type, Subtype string
			Plugins       []struct{ Name string } `json:"plugins"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Type != "system" || ev.Subtype != "init" {
			continue
		}
		for _, w := range want {
			if !slices.ContainsFunc(ev.Plugins, func(p struct{ Name string }) bool { return p.Name == w }) {
				return infra(fmt.Errorf("plugin %s was requested but the session did not load it", w))
			}
		}
		return nil
	}
	return infra(errors.New("no init event in transcript"))
}

// exportSnapshot writes base_commit's tracked files to dir, without the
// repo-relative paths in strip.
func exportSnapshot(ctx context.Context, repo, commit, dir string, strip []string) error {
	archive := exec.CommandContext(ctx, "git", "-C", repo, "archive", "--format=tar", commit)
	extract := exec.CommandContext(ctx, "tar", "-x", "-C", dir)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		return err
	}
	extract.Stdin = pipe
	var stderr bytes.Buffer
	archive.Stderr, extract.Stderr = &stderr, &stderr
	if err := extract.Start(); err != nil {
		return err
	}
	if err := archive.Run(); err != nil {
		return fmt.Errorf("git archive: %w: %s", err, stderr.String())
	}
	if err := extract.Wait(); err != nil {
		return fmt.Errorf("tar: %w: %s", err, stderr.String())
	}
	for _, p := range strip {
		if filepath.IsAbs(p) || !filepath.IsLocal(p) {
			return fmt.Errorf("strip path %q must be inside the repo", p)
		}
		if err := os.RemoveAll(filepath.Join(dir, p)); err != nil {
			return err
		}
	}
	return nil
}

func (a Agent) applyClaudeMD(sandbox string) (variant, hash string, err error) {
	target := filepath.Join(sandbox, "CLAUDE.md")
	switch a.ClaudeMD {
	case "":
		variant = "repo"
	case "none":
		variant = "none"
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return "", "", err
		}
	default:
		variant = variantName(a.ClaudeMD)
		src, err := os.ReadFile(a.ClaudeMD)
		if err != nil {
			return "", "", fmt.Errorf("claude-md variant: %w", err)
		}
		if err := os.WriteFile(target, src, 0o644); err != nil {
			return "", "", err
		}
	}
	if b, err := os.ReadFile(target); err == nil {
		hash = sha(string(b))
	}
	return variant, hash, nil
}

// runSetup runs a.Setup in the snapshot, logging to agent-setup.log.
func (a Agent) runSetup(ctx context.Context, ws Workspace, sandbox string) error {
	if a.Setup == "" {
		return nil
	}
	if err := os.MkdirAll(ws.Artifacts, 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", a.Setup)
	cmd.Dir = sandbox
	cmd.Env = a.env(ws.GoBin)
	out, err := cmd.CombinedOutput()
	if werr := os.WriteFile(filepath.Join(ws.Artifacts, "agent-setup.log"), out, 0o644); werr != nil && err == nil {
		err = werr
	}
	if err != nil {
		return fmt.Errorf("agent setup %q: %w: %s", a.Setup, err, lastLine(string(out)))
	}
	return nil
}

// initSandboxRepo makes dir a fresh repo with one commit tagged eval-base.
// Its message names the real commit the snapshot came from: the sandbox's own
// commit id means nothing outside it, and agents otherwise report it.
func initSandboxRepo(ctx context.Context, dir, source string, ignore []string) error {
	env := append(SafeEnv(),
		"GIT_AUTHOR_NAME=eval", "GIT_AUTHOR_EMAIL=eval@example.test",
		"GIT_COMMITTER_NAME=eval", "GIT_COMMITTER_EMAIL=eval@example.test")
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "init", "-q", "-b", "main")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %w: %s", err, out)
	}
	if len(ignore) > 0 {
		exclude := filepath.Join(dir, ".git", "info", "exclude")
		if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(exclude, []byte(strings.Join(ignore, "\n")+"\n"), 0o644); err != nil {
			return err
		}
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-q", "--no-verify", "-m", "Snapshot of " + source, "-m", "Exported from commit " + source + ". This repository's own commit ids are local to the snapshot; refer to " + source + "."},
		{"tag", "eval-base"},
	} {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w: %s", args, err, out)
		}
	}
	return nil
}

// sandboxDiff returns everything the agent changed relative to the snapshot,
// including new files and anything it committed.
func sandboxDiff(ctx context.Context, dir string) ([]byte, error) {
	if _, err := git(ctx, dir, nil, "add", "-A"); err != nil {
		return nil, err
	}
	return git(ctx, dir, nil, "diff", "--cached", "--binary", "eval-base")
}

func renderPrompt(path string, ws Workspace) (string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("prompt template: %w", err)
	}
	if strings.Contains(string(src), ".Refined") && strings.TrimSpace(ws.Task.Refined) == "" {
		return "", fmt.Errorf("prompt %s needs the task's refined.md, and task %s has none", filepath.Base(path), ws.Task.ID)
	}
	funcs := template.FuncMap{"trimRight": func(s string) string { return strings.TrimRight(s, "\n") }}
	tmpl, err := template.New(filepath.Base(path)).Funcs(funcs).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return "", err
	}
	data := promptData{Task: ws.Task, TestFile: ws.TestFile, Avoid: ws.Avoid,
		Draft: ws.Draft, Decisions: ws.Decisions, Tests: ws.Tests, RedCheck: ws.RedCheck}
	data.TestPrefix, data.HelperPrefix = TestPrefixes(ws.Task.ID)
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// promptData is what prompt templates see: the task's fields, plus the
// test file and name prefixes for test-writing prompts, the mistakes to
// avoid repeating for control-writing ones, the draft and decisions for
// finalise prompts, and the tests and their base results for test reviews.
type promptData struct {
	task.Task
	TestFile     string // e.g. handlers/issue13_low_stock_test.go
	TestPrefix   string // e.g. TestIssue13_
	HelperPrefix string // e.g. h13
	Avoid        []string
	Draft        string
	Decisions    string
	Tests        string
	RedCheck     string
}

var issueNumberPattern = regexp.MustCompile(`^issue-(\d+)`)

// TestPrefixes returns the prefixes a task's hidden tests use for top-level
// test names and helpers, from the issue number in an id like issue-13-…:
// TestIssue13_ and h13. Both are "" for an id without an issue number.
func TestPrefixes(id string) (test, helper string) {
	if m := issueNumberPattern.FindStringSubmatch(id); m != nil {
		return "TestIssue" + m[1] + "_", "h" + m[1]
	}
	return "", ""
}

// deniedReadRules blocks the agent's read tools from the given dirs, using
// Claude Code's absolute-path rule syntax (//abs/path/**).
func deniedReadRules(dirs []string) []string {
	var rules []string
	for _, d := range dirs {
		if d == "" {
			continue
		}
		p := "/" + filepath.ToSlash(filepath.Clean(d)) + "/**"
		for _, tool := range []string{"Read", "Glob", "Grep"} {
			rules = append(rules, tool+"("+p+")")
		}
	}
	return rules
}

// countDeniedRefs counts mentions of denied dirs in the transcript. Read
// rules are not a sandbox (a shell command could still reach outside), so
// this records whether the agent even tried; any non-zero value means the
// attempt should be reviewed before it is trusted.
func countDeniedRefs(transcript string, dirs []string) int {
	b, err := os.ReadFile(transcript)
	if err != nil {
		return 0
	}
	n := 0
	for _, d := range dirs {
		if d != "" {
			n += bytes.Count(b, []byte(filepath.Clean(d)+string(filepath.Separator)))
		}
	}
	return n
}

func variantName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// Fingerprint returns the prompt, CLAUDE.md and plugin hashes an agent
// attempt would record for task t with these settings, without running it:
// the prompt as rendered for t, and the CLAUDE.md the agent would see at
// t's base commit in repo.
func Fingerprint(ctx context.Context, promptPath, claudeMD string, plugins []string, repo string, t task.Task) (promptSHA, claudeMDSHA, pluginsSHA string, err error) {
	prompt, err := renderPrompt(promptPath, Workspace{Task: t})
	if err != nil {
		return "", "", "", err
	}
	switch claudeMD {
	case "none":
	case "":
		if b, gerr := git(ctx, repo, nil, "show", t.BaseCommit+":CLAUDE.md"); gerr == nil {
			claudeMDSHA = sha(string(b))
		}
	default:
		b, rerr := os.ReadFile(claudeMD)
		if rerr != nil {
			return "", "", "", rerr
		}
		claudeMDSHA = sha(string(b))
	}
	if _, _, pluginsSHA, err = describePlugins(plugins); err != nil {
		return "", "", "", err
	}
	return sha(prompt), claudeMDSHA, pluginsSHA, nil
}
