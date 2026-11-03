package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/luizeduardocarvalho/fixlab/internal/implement"
	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

// refineCmd has an agent read the code at a commit and rewrite an issue:
// verified findings, expected behaviour and the product questions only a
// person can answer. It writes a draft for review; nothing is graded.
func refineCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("refine", flag.ExitOnError)
	var c common
	c.register(fs)
	id := fs.String("id", "", "task id the refinement is for (required)")
	issue := fs.String("issue", "", "file holding the issue as filed (required)")
	base := fs.String("base", "", "commit whose code the agent reads (required)")
	out := fs.String("o", "", "where to write the refined issue (default: next to -issue, refined-draft.md)")
	repo := fs.String("repo", task.RepoAPI, "api or web")
	rf := registerReadOnly(fs, "prompts/refine-v1.md")
	_ = fs.Parse(args)

	if *id == "" || *issue == "" || *base == "" {
		fs.Usage()
		return 2
	}
	cfg, err := c.config()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	text, err := os.ReadFile(*issue)
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
		Task: task.Task{ID: *id, Description: string(text), BaseCommit: *base, Repo: *repo},
		Repo: repoDir,
	}
	run, err := runReadOnly(ctx, cfg, rf, "refine", ws)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	path := firstNonEmpty(*out, filepath.Join(filepath.Dir(*issue), "refined-draft.md"))
	if err := os.WriteFile(path, []byte(trimPreamble(run.Answer)+"\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("wrote %s (%s)\n", path, run.Summary)
	return 0
}

// trimPreamble drops any narration before the spec itself ("I've finished
// reading the code..."): everything before the first heading or blockquote.
func trimPreamble(answer string) string {
	lines := strings.Split(answer, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ">") {
			return strings.Join(lines[i:], "\n")
		}
	}
	return answer
}
