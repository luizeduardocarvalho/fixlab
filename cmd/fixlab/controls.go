package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/luizeduardocarvalho/fixlab/internal/implement"
	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

// controlsCmd has an agent write plausible wrong fixes (controls) from the
// task's reference fix and keeps each one the hidden tests catch, as
// controls/pN-<slug>.diff. A control that passes every hidden test means the
// tests miss a realistic mistake: it is reported, not kept, and the command
// fails so a person looks at the tests.
func controlsCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("controls", flag.ExitOnError)
	var c common
	c.register(fs)
	taskPath := fs.String("task", "", "task file with a reference_commit (required)")
	n := fs.Int("n", 2, "controls to write, one agent session each")
	model := fs.String("model", "", "agent model (default: the CLI's default)")
	effort := fs.String("effort", "", "agent effort level")
	prompt := fs.String("prompt", "prompts/write-control-v1.md", "prompt template, relative to -workspace")
	claudeMD := fs.String("claude-md", "", `CLAUDE.md the agent sees: "" = the repo's, "none", or a file`)
	agentCLI := fs.String("agent-cli", firstNonEmpty(os.Getenv("CLAUDE_BIN"), "claude"), "claude binary (env CLAUDE_BIN)")
	agentConfig := fs.String("agent-config-dir", os.Getenv("CLAUDE_CONFIG_DIR"), "CLAUDE_CONFIG_DIR for the agent's account")
	budget := fs.Float64("agent-budget-usd", 5, "per-session limit at API list price (0 = none)")
	_ = fs.Parse(args)

	if *taskPath == "" || *n < 1 {
		fs.Usage()
		return 2
	}
	cfg, err := c.config()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	t, err := task.Read(*taskPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if t.ReferenceCommit == "" {
		fmt.Fprintf(os.Stderr, "task %s has no reference_commit; promote a passing attempt first\n", t.ID)
		return 2
	}
	if t.Repo != task.RepoAPI {
		fmt.Fprintln(os.Stderr, "controls supports only api tasks")
		return 2
	}
	tests := map[string][]byte{}
	for _, h := range t.HiddenTests {
		if tests[h.To], err = os.ReadFile(filepath.Join(t.Dir, h.From)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	prefix, _ := implement.TestPrefixes(t.ID)
	if prefix == "" {
		fmt.Fprintf(os.Stderr, "task id %s has no issue number, so its hidden tests have no prefix\n", t.ID)
		return 2
	}
	controlsDir := filepath.Join(t.Dir, "controls")
	existing, err := readControls(controlsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	promptPath := *prompt
	if !filepath.IsAbs(promptPath) {
		promptPath = filepath.Join(cfg.Workspace, promptPath)
	}
	mdPath := *claudeMD
	if mdPath != "" && mdPath != "none" {
		if mdPath, err = filepath.Abs(mdPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	p, cleanup, err := taskProfile(ctx, cfg, t)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer cleanup()
	agent := implement.Agent{
		CLI: *agentCLI, ConfigDir: *agentConfig, Model: *model, Effort: *effort,
		PromptPath: promptPath, ClaudeMD: mdPath, BudgetUSD: *budget, Timeout: 30 * time.Minute,
		ProjectTools: p.AgentTools(),
		DeniedPaths:  deniedDirs(cfg, resolveHistory("")),
		Strip:        snapshotStrip(cfg),
	}

	kept, missed, broken := 0, 0, 0
	for i := 1; i <= *n; i++ {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "interrupted")
			return 130
		}
		var avoid []string
		for _, ctl := range existing {
			avoid = append(avoid, ctl.mistake)
		}
		runID := fmt.Sprintf("%s-control-%s", t.ID, time.Now().UTC().Format("20060102T150405Z"))
		artifacts := filepath.Join(cfg.OutDir, t.ID, runID)
		ws := implement.Workspace{Task: t, Repo: cfg.APIRepo, Artifacts: artifacts, Avoid: avoid}
		fmt.Fprintf(os.Stderr, "== %s control %d/%d\n", t.ID, i, *n)
		diff, answer, info, err := agent.WriteControl(ctx, ws)
		if err != nil {
			fmt.Fprintf(os.Stderr, "control %d: %v\n", i, err)
			broken++
			continue
		}
		slug, mistake := parseControlAnswer(answer)
		if err := os.WriteFile(filepath.Join(artifacts, "control.diff"), diff, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		tok := info.InputTokens + info.OutputTokens + info.CacheReadTokens + info.CacheWriteTokens
		fmt.Printf("control %d (%s, %d turns, %.2fM tokens): %s\n", i, slug, info.Turns, float64(tok)/1e6, mistake)

		results, err := hiddenCheck(ctx, cfg.APIRepo, cfg.WorkDir, t.BaseCommit, diff, tests, prefix, filepath.Join(artifacts, "hidden-check.json"))
		if err != nil {
			fmt.Printf("  dropped: it doesn't build or vet (%v)\n", firstLine(err.Error()))
			broken++
			continue
		}
		var failed []string
		for name, action := range results {
			if action != "pass" {
				failed = append(failed, strings.TrimPrefix(name, prefix))
			}
		}
		sort.Strings(failed)
		if len(failed) == 0 {
			fmt.Printf("  MISSED: passes all %d hidden tests; the tests don't catch this mistake (diff: %s)\n", len(results), filepath.Join(artifacts, "control.diff"))
			missed++
			existing = append(existing, control{mistake: mistake})
			continue
		}
		name := fmt.Sprintf("p%d-%s.diff", len(existing)+1, slug)
		header := fmt.Sprintf("# mistake: %s\n# fails: %s (%d of %d hidden tests)\n", mistake, strings.Join(failed, ", "), len(failed), len(results))
		if err := os.MkdirAll(controlsDir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.WriteFile(filepath.Join(controlsDir, name), append([]byte(header), diff...), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("  kept as controls/%s: fails %s\n", name, strings.Join(failed, ", "))
		existing = append(existing, control{mistake: mistake})
		kept++
	}
	fmt.Printf("%s: %d controls kept, %d missed by the hidden tests, %d dropped\n", t.ID, kept, missed, broken)
	if missed > 0 || kept == 0 {
		return 1
	}
	return 0
}

type control struct{ mistake string }

// readControls returns the controls already in dir. A control's mistake is
// its "# mistake:" header, or its file name for controls written by hand.
func readControls(dir string) ([]control, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.diff"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []control
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		mistake := strings.TrimSuffix(filepath.Base(p), ".diff")
		sc := bufio.NewScanner(bytes.NewReader(src))
		for sc.Scan() && strings.HasPrefix(sc.Text(), "#") {
			if m, ok := strings.CutPrefix(sc.Text(), "# mistake: "); ok {
				mistake = m
			}
		}
		out = append(out, control{mistake: mistake})
	}
	return out, nil
}

var slugClean = regexp.MustCompile(`[^a-z0-9]+`)

// parseControlAnswer reads the session's "slug:" and "mistake:" lines.
func parseControlAnswer(answer string) (slug, mistake string) {
	for _, line := range strings.Split(answer, "\n") {
		line = strings.Trim(strings.TrimSpace(line), "`")
		if v, ok := strings.CutPrefix(line, "slug:"); ok {
			slug = strings.Trim(slugClean.ReplaceAllString(strings.ToLower(strings.TrimSpace(v)), "-"), "-")
		}
		if v, ok := strings.CutPrefix(line, "mistake:"); ok {
			mistake = strings.TrimSpace(v)
		}
	}
	if slug == "" {
		slug = "control"
	}
	if mistake == "" {
		mistake = firstLine(strings.TrimSpace(answer))
	}
	return slug, mistake
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
