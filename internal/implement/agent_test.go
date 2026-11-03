package implement

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

// fakeClaude stands in for the CLI. It records its args and environment
// facts the test checks, edits and commits like an agent might, then prints
// stream-json init and result events.
const fakeClaude = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "9.9.9 (Claude Code)"; exit 0; fi
printf '%s\n' "$@" > "$FAKE_OUT/args"
pwd > "$FAKE_OUT/cwd"
git log --oneline | wc -l | tr -d ' ' > "$FAKE_OUT/commits"
git branch -a > "$FAKE_OUT/branches"
ls > "$FAKE_OUT/files"
[ -f CLAUDE.md ] && cp CLAUDE.md "$FAKE_OUT/claude_md"
[ -d gout ] && echo "agent touched the graph" >> gout/graph.json
[ -f gout/graph.json ] && cp gout/graph.json "$FAKE_OUT/graph"
echo "fixed" > app.go
echo "new" > new_test.go
git add -A && git -c user.name=a -c user.email=a@b commit -qm agent
echo '{"type":"system","subtype":"init","model":"claude-test-1","session_id":"s-1","plugins":[{"name":"builtin-x"},{"name":"kit"}]}'
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","input":{"file_path":"/secret/repo/eval/x"}}]}}'
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Skill","input":{"skill":"kit:fix-root-causes"}},{"type":"tool_use","name":"Skill","input":{"skill":"kit:fix-root-causes"}}]}}'
echo '{"type":"result","subtype":"success","total_cost_usd":0.42,"num_turns":7,"duration_ms":12500,"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200}}'
`

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAgent_ImplementInIsolatedSnapshot(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	write(t, filepath.Join(repo, "app.go"), "broken\n")
	write(t, filepath.Join(repo, "CLAUDE.md"), "repo instructions\n")
	write(t, filepath.Join(repo, "eval", "tasks", "x", "testdata", "hidden_test.go"), "secret\n")
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "base")
	base := gitRun(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "checkout", "-qb", "fix/the-answer")
	write(t, filepath.Join(repo, "app.go"), "reference fix\n")
	gitRun(t, repo, "commit", "-qam", "reference")
	gitRun(t, repo, "checkout", "-q", "main")
	worktree := filepath.Join(root, "cand")
	gitRun(t, repo, "worktree", "add", "-q", "--detach", worktree, base)

	out := filepath.Join(root, "out")
	write(t, filepath.Join(out, ".keep"), "")
	cli := filepath.Join(root, "claude")
	write(t, cli, fakeClaude)
	if err := os.Chmod(cli, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_OUT", out)
	prompt := filepath.Join(root, "issue-v1.md")
	write(t, prompt, "Fix {{.ID}}: {{.Description}}")
	variant := filepath.Join(root, "claude-md-short.md")
	write(t, variant, "short instructions\n")

	plugin := filepath.Join(root, "kit")
	write(t, filepath.Join(plugin, ".claude-plugin", "plugin.json"), `{"name":"kit","version":"1.2.3"}`)
	write(t, filepath.Join(plugin, "skills", "fix-root-causes", "SKILL.md"), "fix it at the root\n")
	a := Agent{CLI: cli, Model: "sonnet", Effort: "high", PromptPath: prompt, ClaudeMD: variant,
		BudgetUSD: 2, ProjectTools: []string{"Bash(go test:*)"}, DeniedPaths: []string{"/secret/repo"}, Strip: []string{"eval"}, Plugins: []string{plugin},
		Setup: "mkdir -p gout && echo built > gout/graph.json", Ignore: []string{"gout/"}}
	ws := Workspace{Task: task.Task{ID: "t1", Description: "the bug", BaseCommit: base},
		Repo: repo, Worktree: worktree, Artifacts: filepath.Join(root, "artifacts")}
	info, err := a.Implement(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}

	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(out, name))
		return strings.TrimSpace(string(b))
	}
	// Isolation: one commit, no other branches, no workspace (eval/), outside the repo.
	if read("commits") != "1" {
		t.Fatalf("sandbox history: want 1 commit, got %q", read("commits"))
	}
	if strings.Contains(read("branches"), "the-answer") {
		t.Fatal("sandbox exposes the reference branch")
	}
	if strings.Contains(read("files"), "eval") {
		t.Fatal("sandbox contains the workspace, eval/")
	}
	if cwd := read("cwd"); cwd == "" || strings.HasPrefix(cwd, repo) {
		t.Fatalf("sandbox must be outside the repo, got %q", cwd)
	}
	if read("claude_md") != "short instructions" {
		t.Fatalf("CLAUDE.md variant not applied: %q", read("claude_md"))
	}
	args := read("args")
	for _, want := range []string{"Fix t1: the bug", "stream-json", "dontAsk", "--setting-sources\nproject", "--model\nsonnet", "--effort\nhigh", "--max-budget-usd\n2.00", "Read(//secret/repo/**)", "--plugin-dir\n" + plugin, "git status:*),Bash(go test:*),Skill", "--tools\nRead,Edit,Write,Glob,Grep,Bash,Skill\n"} {
		if !strings.Contains(args, want) {
			t.Fatalf("CLI args missing %q:\n%s", want, args)
		}
	}

	// The agent's committed change, including new files, reached the worktree.
	if b, _ := os.ReadFile(filepath.Join(worktree, "app.go")); strings.TrimSpace(string(b)) != "fixed" {
		t.Fatalf("change not applied: %q", b)
	}
	if _, err := os.Stat(filepath.Join(worktree, "new_test.go")); err != nil {
		t.Fatal("new file not applied")
	}
	// Setup ran before the agent, and what it generated stayed out of the diff.
	if !strings.HasPrefix(read("graph"), "built") {
		t.Fatalf("setup output not visible to the agent: %q", read("graph"))
	}
	if _, err := os.Stat(filepath.Join(worktree, "gout")); !os.IsNotExist(err) {
		t.Fatal("ignored setup output leaked into the candidate")
	}
	if b, _ := os.ReadFile(filepath.Join(worktree, "CLAUDE.md")); strings.TrimSpace(string(b)) != "repo instructions" {
		t.Fatal("CLAUDE.md variant leaked into the candidate diff")
	}
	for _, f := range []string{"prompt.md", "transcript.jsonl", "candidate.diff"} {
		if _, err := os.Stat(filepath.Join(ws.Artifacts, f)); err != nil {
			t.Fatalf("artifact %s missing", f)
		}
	}

	want := Info{Kind: "agent", Model: "claude-test-1", Effort: "high", CLIVersion: "9.9.9 (Claude Code)",
		PromptVariant: "issue-v1", ClaudeMDVariant: "claude-md-short", CostUSD: 0.42, InputTokens: 100,
		OutputTokens: 50, CacheReadTokens: 1000, CacheWriteTokens: 200, Turns: 7, DurationSec: 12.5,
		Outcome: "success", SessionID: "s-1", DeniedPathRefs: 1, Plugins: "kit@1.2.3", SkillsUsed: "kit:fix-root-causes",
		Setup: "mkdir -p gout && echo built > gout/graph.json"}
	want.PromptSHA256, want.ClaudeMDSHA256, want.PluginsSHA256 = info.PromptSHA256, info.ClaudeMDSHA256, info.PluginsSHA256
	if len(info.PluginsSHA256) != 64 {
		t.Fatalf("plugin hash missing: %q", info.PluginsSHA256)
	}
	if info != want {
		t.Fatalf("info:\n got %+v\nwant %+v", info, want)
	}
	if info.PromptSHA256 != sha("Fix t1: the bug") || info.ClaudeMDSHA256 != sha("short instructions\n") {
		t.Fatal("hashes don't match what the agent saw")
	}
}

func TestAgent_MissingCLIIsInfraError(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	write(t, filepath.Join(repo, "a.go"), "x\n")
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "base")
	prompt := filepath.Join(root, "p.md")
	write(t, prompt, "{{.Description}}")
	a := Agent{CLI: filepath.Join(root, "no-such-claude"), PromptPath: prompt}
	_, err := a.Implement(context.Background(), Workspace{Task: task.Task{BaseCommit: "HEAD", Description: "d"}, Repo: repo, Artifacts: filepath.Join(root, "art")})
	var ie InfraError
	if err == nil || !errors.As(err, &ie) {
		t.Fatalf("want InfraError, got %v", err)
	}
}

func TestCheckPluginsLoaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	write(t, path, `{"type":"system","subtype":"init","plugins":[{"name":"builtin-x"}]}`+"\n")
	var ie InfraError
	if err := checkPluginsLoaded(path, []string{"pstack"}); err == nil || !errors.As(err, &ie) {
		t.Fatalf("want infra error for a plugin that did not load, got %v", err)
	}
	if err := checkPluginsLoaded(path, nil); err != nil {
		t.Fatalf("no plugins requested: %v", err)
	}
}

func TestAgent_ReadOnly(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	write(t, filepath.Join(repo, "app.go"), "package app\n")
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "base")
	base := gitRun(t, repo, "rev-parse", "HEAD")
	prompt := filepath.Join(root, "refine.md")
	write(t, prompt, "Refine: {{.Description}}")

	refine := func(script string) (string, error) {
		cli := filepath.Join(root, "claude")
		write(t, cli, script)
		if err := os.Chmod(cli, 0o755); err != nil {
			t.Fatal(err)
		}
		a := Agent{CLI: cli, PromptPath: prompt, ProjectTools: []string{"Bash(go test:*)"}}
		ws := Workspace{Task: task.Task{ID: "t", Description: "the issue", BaseCommit: base}, Repo: repo, Artifacts: filepath.Join(root, "art")}
		answer, _, err := a.ReadOnly(context.Background(), ws)
		return answer, err
	}
	const events = `echo '{"type":"system","subtype":"init","model":"m","session_id":"s"}'
printf '%s\n' '{"type":"result","subtype":"success","result":"## Expected behaviour\n- 404","total_cost_usd":0.1,"num_turns":2,"duration_ms":1000,"usage":{}}'
`
	readOnly := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 9; exit 0; fi\nprintf '%s\\n' \"$@\" > " + filepath.Join(root, "args") + "\n" + events
	answer, err := refine(readOnly)
	if err != nil || !strings.Contains(answer, "Expected behaviour") {
		t.Fatalf("got %q, %v", answer, err)
	}
	args, _ := os.ReadFile(filepath.Join(root, "args"))
	if !strings.Contains(string(args), "--allowedTools\nRead,Glob,Grep\n") {
		t.Fatalf("refine must run with read-only tools, got:\n%s", args)
	}
	// A session that edits code is refused, whatever it answers.
	if _, err := refine("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 9; exit 0; fi\necho changed > app.go\n" + events); err == nil || !strings.Contains(err.Error(), "must only read") {
		t.Fatalf("want a refused edit, got %v", err)
	}
}

// A refined prompt renders the task's refined.md, and refuses a task
// without one.
func TestRenderPrompt_Refined(t *testing.T) {
	tk, err := task.Read("../../testdata/example/tasks/issue-1-scope-lookups/task.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got, err := renderPrompt("../../testdata/example/prompts/refined-v1.md", Workspace{Task: tk})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"not scoped to the caller's team", "returns 404 for an item of another team.\n---"} {
		if !strings.Contains(got, want) {
			t.Errorf("refined prompt lacks %q:\n%s", want, got)
		}
	}
	tk.Refined = ""
	if _, err := renderPrompt("../../testdata/example/prompts/refined-v1.md", Workspace{Task: tk}); err == nil || !strings.Contains(err.Error(), "refined.md") {
		t.Fatalf("want an error for a task without refined.md, got %v", err)
	}
}

func TestAgent_WriteTests(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	write(t, filepath.Join(repo, "app.go"), "package app\n")
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "base")
	base := gitRun(t, repo, "rev-parse", "HEAD")
	prompt := filepath.Join(root, "write.md")
	write(t, prompt, "Write {{.TestFile}} with {{.TestPrefix}} tests and {{.HelperPrefix}} helpers for: {{.Refined}}")

	run := func(script string) ([]byte, error) {
		cli := filepath.Join(root, "claude")
		write(t, cli, script)
		if err := os.Chmod(cli, 0o755); err != nil {
			t.Fatal(err)
		}
		a := Agent{CLI: cli, PromptPath: prompt}
		ws := Workspace{Task: task.Task{ID: "issue-13-x", BaseCommit: base, Refined: "the spec"}, Repo: repo,
			Artifacts: filepath.Join(root, "art"), TestFile: "pkg/x_test.go"}
		src, _, err := a.WriteTests(context.Background(), ws)
		return src, err
	}
	const events = `echo '{"type":"system","subtype":"init","model":"m","session_id":"s"}'
printf '%s\n' '{"type":"result","subtype":"success","result":"done","total_cost_usd":0.1,"num_turns":2,"duration_ms":1000,"usage":{}}'
`
	head := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 9; exit 0; fi\nprintf '%s\\n' \"$@\" > " + filepath.Join(root, "args") + "\n"
	src, err := run(head + "mkdir -p pkg && echo 'package x_test' > pkg/x_test.go\n" + events)
	if err != nil || strings.TrimSpace(string(src)) != "package x_test" {
		t.Fatalf("got %q, %v", src, err)
	}
	args, _ := os.ReadFile(filepath.Join(root, "args"))
	if !strings.Contains(string(args), "Write pkg/x_test.go with TestIssue13_ tests and h13 helpers for: the spec") {
		t.Fatalf("prompt not rendered with the test file and prefixes:\n%s", args)
	}
	if _, err := run(head + "mkdir -p pkg && echo 'package x_test' > pkg/x_test.go && echo changed > app.go\n" + events); err == nil || !strings.Contains(err.Error(), "other than") {
		t.Fatalf("want a refused code change, got %v", err)
	}
	if _, err := run(head + events); err == nil || !strings.Contains(err.Error(), "did not write") {
		t.Fatalf("want an error when no test file is written, got %v", err)
	}
}

func TestAgent_WriteControl(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	write(t, filepath.Join(repo, "app.go"), "package app\n")
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "base")
	base := gitRun(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "app.go"), "package app\n\nconst limit = 5\n")
	write(t, filepath.Join(repo, "app_hidden_test.go"), "package app\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "fix")
	ref := gitRun(t, repo, "rev-parse", "HEAD")
	prompt := filepath.Join(root, "control.md")
	write(t, prompt, "Break it. Avoid:{{range .Avoid}} {{.}};{{end}}")

	run := func(script string) ([]byte, string, error) {
		cli := filepath.Join(root, "claude")
		write(t, cli, script)
		if err := os.Chmod(cli, 0o755); err != nil {
			t.Fatal(err)
		}
		a := Agent{CLI: cli, PromptPath: prompt}
		ws := Workspace{Task: task.Task{ID: "issue-13-x", BaseCommit: base, ReferenceCommit: ref,
			HiddenTests: []task.HiddenTest{{From: "testdata/h.go", To: "app_hidden_test.go"}}},
			Repo: repo, Artifacts: filepath.Join(root, "art"), Avoid: []string{"off by one"}}
		diff, answer, _, err := a.WriteControl(context.Background(), ws)
		return diff, answer, err
	}
	head := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 9; exit 0; fi\nprintf '%s\\n' \"$@\" > " + filepath.Join(root, "args") + "\n" +
		"[ -f app_hidden_test.go ] && echo leaked > " + filepath.Join(root, "leak") + "\n"
	const events = `echo '{"type":"system","subtype":"init","model":"m","session_id":"s"}'
printf '%s\n' '{"type":"result","subtype":"success","result":"slug: x\nmistake: limit is 6","total_cost_usd":0.1,"num_turns":2,"duration_ms":1000,"usage":{}}'
`
	diff, answer, err := run(head + "grep -q 'limit = 5' app.go && sed -i.bak 's/5/6/' app.go && rm app.go.bak\n" + events)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "leak")); err == nil {
		t.Fatal("the session saw the hidden test file")
	}
	if !strings.Contains(string(diff), "+const limit = 6") || strings.Contains(string(diff), "app_hidden_test.go") {
		t.Fatalf("want a diff from base with the mistake and without hidden tests, got:\n%s", diff)
	}
	if !strings.Contains(answer, "mistake: limit is 6") {
		t.Errorf("answer = %q", answer)
	}
	if args, _ := os.ReadFile(filepath.Join(root, "args")); !strings.Contains(string(args), "Avoid: off by one;") {
		t.Errorf("prompt not rendered with Avoid:\n%s", args)
	}
	if _, _, err := run(head + events); err == nil || !strings.Contains(err.Error(), "unchanged") {
		t.Fatalf("want an error for an unchanged fix, got %v", err)
	}
	if _, _, err := run(head + "echo 'package app' > app_test.go\n" + events); err == nil || !strings.Contains(err.Error(), "test file") {
		t.Fatalf("want a refused test change, got %v", err)
	}
}

func TestAgent_APIErrorIsInfra(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	write(t, filepath.Join(repo, "app.go"), "package app\n")
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "base")
	base := gitRun(t, repo, "rev-parse", "HEAD")
	prompt := filepath.Join(root, "refine.md")
	write(t, prompt, "Refine: {{.Description}}")
	cli := filepath.Join(root, "claude")
	write(t, cli, `#!/bin/sh
if [ "$1" = "--version" ]; then echo 9; exit 0; fi
echo '{"type":"system","subtype":"init","model":"m","session_id":"s"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","result":"API Error: Cannot reach the API server (ENOTFOUND)","num_turns":1,"duration_ms":1000,"usage":{}}'
`)
	if err := os.Chmod(cli, 0o755); err != nil {
		t.Fatal(err)
	}
	a := Agent{CLI: cli, PromptPath: prompt}
	ws := Workspace{Task: task.Task{ID: "t", Description: "the issue", BaseCommit: base}, Repo: repo, Artifacts: filepath.Join(root, "art")}
	_, _, err := a.ReadOnly(context.Background(), ws)
	var ie InfraError
	if !errors.As(err, &ie) || !strings.Contains(err.Error(), "ENOTFOUND") {
		t.Fatalf("want an infra error naming the API failure, got %v", err)
	}
}

// The finalise and review-tests prompts render with the data their commands
// pass, and fail loudly if a field is renamed.
func TestRenderPrompt_FinaliseAndReviewTests(t *testing.T) {
	tk := task.Task{ID: "issue-28-x", Repo: task.RepoAPI, BaseCommit: "abc1234def5678abc1234def5678abc1234def56",
		Description: "the issue", Refined: "the spec"}
	got, err := renderPrompt("../../testdata/example/prompts/finalise-v1.md", Workspace{Task: tk, Draft: "the draft", Decisions: "- 28-1: q\n  Answer: a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"the issue", "the draft", "- 28-1: q", "code at abc1234, read-only", "## Editorial notes"} {
		if !strings.Contains(got, want) {
			t.Errorf("finalise prompt lacks %q", want)
		}
	}
	got, err = renderPrompt("../../testdata/example/prompts/review-tests-v1.md", Workspace{Task: tk, Tests: "func TestIssue28_X(t *testing.T) {}", RedCheck: "fail TestIssue28_X"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"the spec", "func TestIssue28_X", "fail TestIssue28_X", "named `TestIssue28_…`", "start with `h28`"} {
		if !strings.Contains(got, want) {
			t.Errorf("review-tests prompt lacks %q", want)
		}
	}
}

// Agent sessions, and the test code they run, must not inherit production or
// cloud credentials, nor find the user's gcloud and Pulumi logins.
func TestAgentEnv_DropsProductionCredentials(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@prod.example/db")
	t.Setenv("CLERK_SECRET_KEY", "sk_live_x")
	t.Setenv("PULUMI_ACCESS_TOKEN", "pul-x")
	t.Setenv("CLOUDSDK_CONFIG", "/real/gcloud")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/real/adc.json")
	t.Setenv("GH_TOKEN", "ghp_x")
	t.Setenv("ANTHROPIC_API_KEY", "keep-me")
	t.Setenv("DATABASE_URL_NOTE", "unrelated, kept")

	got := map[string][]string{}
	for _, kv := range (Agent{}).env("") {
		name, value, _ := strings.Cut(kv, "=")
		got[name] = append(got[name], value)
	}
	for _, name := range []string{"DATABASE_URL", "CLERK_SECRET_KEY", "PULUMI_ACCESS_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS", "GH_TOKEN"} {
		if v, ok := got[name]; ok {
			t.Errorf("%s leaked into the agent env: %v", name, v)
		}
	}
	for _, name := range []string{"CLOUDSDK_CONFIG", "PULUMI_HOME"} {
		if v := got[name]; len(v) != 1 || v[0] != noCloudDir() {
			t.Errorf("%s = %v, want only the empty dir %s", name, v, noCloudDir())
		}
	}
	// gcloud may leave empty bookkeeping files there; it holds no login.
	if info, err := os.Stat(noCloudDir()); err != nil || !info.IsDir() {
		t.Errorf("no-cloud dir should exist: %v", err)
	}
	for _, name := range []string{"ANTHROPIC_API_KEY", "DATABASE_URL_NOTE"} {
		if len(got[name]) != 1 {
			t.Errorf("%s should be kept, got %v", name, got[name])
		}
	}
}
