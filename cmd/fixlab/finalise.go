package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/luizeduardocarvalho/fixlab/internal/implement"
	"github.com/luizeduardocarvalho/fixlab/internal/runner"
	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

// readOnlyFlags are the agent flags of the read-only commands (refine,
// finalise, review-tests).
type readOnlyFlags struct {
	model, effort, prompt, claudeMD, cli, configDir *string
	budget                                          *float64
}

func registerReadOnly(fs *flag.FlagSet, prompt string) readOnlyFlags {
	return readOnlyFlags{
		model:     fs.String("model", "", "agent model (default: the CLI's default)"),
		effort:    fs.String("effort", "", "agent effort level"),
		prompt:    fs.String("prompt", prompt, "prompt template, relative to -workspace"),
		claudeMD:  fs.String("claude-md", "", `CLAUDE.md the agent sees: "" = the repo's, "none", or a file`),
		cli:       fs.String("agent-cli", firstNonEmpty(os.Getenv("CLAUDE_BIN"), "claude"), "claude binary (env CLAUDE_BIN)"),
		configDir: fs.String("agent-config-dir", os.Getenv("CLAUDE_CONFIG_DIR"), "CLAUDE_CONFIG_DIR for the agent's account"),
		budget:    fs.Float64("agent-budget-usd", 5, "session limit at API list price (0 = none)"),
	}
}

// agent builds the read-only agent. It can't read the eval dir, the history
// or sibling checkouts, so it sees only the code at the base and the prompt.
func (f readOnlyFlags) agent(cfg runner.Config) (implement.Agent, error) {
	promptPath := *f.prompt
	if !filepath.IsAbs(promptPath) {
		promptPath = filepath.Join(cfg.Workspace, promptPath)
	}
	mdPath := *f.claudeMD
	if mdPath != "" && mdPath != "none" {
		var err error
		if mdPath, err = filepath.Abs(mdPath); err != nil {
			return implement.Agent{}, err
		}
	}
	return implement.Agent{
		CLI: *f.cli, ConfigDir: *f.configDir, Model: *f.model, Effort: *f.effort,
		PromptPath: promptPath, ClaudeMD: mdPath, BudgetUSD: *f.budget, Timeout: 30 * time.Minute,
		DeniedPaths: deniedDirs(cfg, resolveHistory("")),
		Strip:       snapshotStrip(cfg),
	}, nil
}

// readOnlyRun is what a read-only session produced.
type readOnlyRun struct {
	Answer    string // the agent's final message
	Artifacts string // results/<id>/<id>-<kind>-<time>/: prompt and transcript
	Summary   string // turns, tokens and where the transcript is, for the log
}

// runReadOnly runs one read-only session, saving its prompt and transcript
// under results/<id>/<id>-<kind>-<time>/.
func runReadOnly(ctx context.Context, cfg runner.Config, f readOnlyFlags, kind string, ws implement.Workspace) (readOnlyRun, error) {
	agent, err := f.agent(cfg)
	if err != nil {
		return readOnlyRun{}, err
	}
	runID := fmt.Sprintf("%s-%s-%s", ws.Task.ID, kind, time.Now().UTC().Format("20060102T150405Z"))
	ws.Artifacts = filepath.Join(cfg.OutDir, ws.Task.ID, runID)
	answer, info, err := agent.ReadOnly(ctx, ws)
	if err != nil {
		return readOnlyRun{}, err
	}
	tok := info.InputTokens + info.OutputTokens + info.CacheReadTokens + info.CacheWriteTokens
	return readOnlyRun{
		Answer:    answer,
		Artifacts: ws.Artifacts,
		Summary:   fmt.Sprintf("%d turns, %.2fM tokens, %d denied-path refs; transcript in %s", info.Turns, float64(tok)/1e6, info.DeniedPathRefs, ws.Artifacts),
	}, nil
}

// decision is one entry of the decisions file the decision page exports.
type decision struct {
	ID       string `json:"id"`
	Issue    any    `json:"issue"`
	Question string `json:"question"`
	Decided  bool   `json:"decided"`
	Answer   string `json:"answer"`
	Followed bool   `json:"followedRecommendation"`
	Note     string `json:"note"`
}

var issueNumber = regexp.MustCompile(`^issue-(\d+)-`)

// issueDecisions returns the decisions for issue n and the plan-wide ones, as
// the text a finalise prompt shows. Every one of issue n's questions must be
// decided, and there must be at least one.
func issueDecisions(path, n string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var file struct {
		Answers []decision `json:"answers"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	var b strings.Builder
	var own int
	var undecided []string
	for _, d := range file.Answers {
		issue := fmt.Sprint(d.Issue)
		if issue != n && issue != "plan" {
			continue
		}
		if !d.Decided {
			undecided = append(undecided, d.ID)
			continue
		}
		if issue == n {
			own++
		}
		fmt.Fprintf(&b, "- %s: %s\n  Answer: %s", d.ID, d.Question, d.Answer)
		if !d.Followed {
			b.WriteString(" (not the draft's recommendation)")
		}
		if d.Note != "" {
			fmt.Fprintf(&b, "\n  Maintainer's note: %s", d.Note)
		}
		b.WriteString("\n")
	}
	if len(undecided) > 0 {
		return "", fmt.Errorf("%s: undecided: %s", path, strings.Join(undecided, ", "))
	}
	if own == 0 {
		return "", fmt.Errorf("%s has no decisions for issue %s", path, n)
	}
	return b.String(), nil
}

// editorialHeading starts the part of a finalise answer that is notes for
// the maintainer, not spec.
const editorialHeading = "## Editorial notes"

// finaliseCmd has an agent turn a task's refine draft and the maintainer's
// decisions into refined.md, the spec tests are written from and the
// refine-then-implement strategy shows. It writes refined.md and, when the
// agent made editorial choices, finalise-notes.md next to it.
func finaliseCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("finalise", flag.ExitOnError)
	var c common
	c.register(fs)
	dir := fs.String("task-dir", "", "task directory holding issue-as-filed.md and refined-draft.md (required)")
	decisions := fs.String("decisions", "", "decisions JSON exported by the decision page (required)")
	base := fs.String("base", "", "commit whose code the agent reads (required)")
	repo := fs.String("repo", task.RepoAPI, "api or web")
	force := fs.Bool("force", false, "overwrite an existing refined.md")
	rf := registerReadOnly(fs, "prompts/finalise-v1.md")
	_ = fs.Parse(args)

	if *dir == "" || *decisions == "" || *base == "" {
		fs.Usage()
		return 2
	}
	cfg, err := c.config()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	taskDir, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	id := filepath.Base(taskDir)
	m := issueNumber.FindStringSubmatch(id)
	if m == nil {
		fmt.Fprintf(os.Stderr, "task dir %s is not named issue-<n>-<slug>\n", id)
		return 2
	}
	out := filepath.Join(taskDir, "refined.md")
	if _, err := os.Stat(out); err == nil && !*force {
		fmt.Fprintf(os.Stderr, "%s exists; pass -force to overwrite it\n", out)
		return 2
	}
	issue, err := os.ReadFile(filepath.Join(taskDir, "issue-as-filed.md"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	draft, err := os.ReadFile(filepath.Join(taskDir, "refined-draft.md"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "finalise needs the refine draft:", err)
		return 2
	}
	decided, err := issueDecisions(*decisions, m[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	repoDir, err := cfg.RepoFor(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ws := implement.Workspace{
		Task:  task.Task{ID: id, Description: string(issue), BaseCommit: *base, Repo: *repo},
		Repo:  repoDir,
		Draft: string(draft), Decisions: decided,
	}
	run, err := runReadOnly(ctx, cfg, rf, "finalise", ws)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	spec, notes, _ := strings.Cut(trimPreamble(run.Answer), editorialHeading)
	if err := os.WriteFile(out, []byte(strings.TrimSpace(spec)+"\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("wrote %s (%s)\n", out, run.Summary)
	if notes = strings.TrimSpace(notes); notes != "" {
		notesPath := filepath.Join(taskDir, "finalise-notes.md")
		if err := os.WriteFile(notesPath, []byte(editorialHeading+"\n\n"+notes+"\n"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("editorial notes for review in %s\n", notesPath)
	}
	return 0
}
