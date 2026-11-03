package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/luizeduardocarvalho/fixlab/internal/profile"
	"github.com/luizeduardocarvalho/fixlab/internal/result"
	"github.com/luizeduardocarvalho/fixlab/internal/runner"
)

const envUsage = `usage: fixlab env up|down|check -project DIR

  up     build the project's app from DIR as it is and start its stack (services,
         app, seed), with the app published on 127.0.0.1; replaces a running one
  down   remove the stack
  check  prove DIR/.eval/profile.yaml works: every stage passes, the stack
         comes up and seeds, one load round runs clean; then removes the stack`

func envCmd(ctx context.Context, args []string) int {
	if len(args) == 0 || (args[0] != "up" && args[0] != "down" && args[0] != "check") {
		fmt.Fprintln(os.Stderr, envUsage)
		return 2
	}
	action, args := args[0], args[1:]
	fs := flag.NewFlagSet("env "+action, flag.ExitOnError)
	project := fs.String("project", "", "project root holding .eval/profile.yaml (required)")
	goBin := fs.String("go", firstNonEmpty(os.Getenv("GO"), "go"), "go binary")
	_ = fs.Parse(args)
	if *project == "" {
		fmt.Fprintln(os.Stderr, envUsage)
		return 2
	}
	dir, err := filepath.Abs(*project)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	p, err := profile.Read(filepath.Join(dir, profile.Dir))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	root, err := runner.EnvRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	env := runner.Env{
		Profile: p, Project: dir, GoBin: *goBin,
		Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
		Dir:  filepath.Join(root, runner.EnvID(p, dir)),
	}

	switch action {
	case "up":
		info, err := env.Up(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("%s is up at %s\n", p.Name, info.URL)
		names := make([]string, 0, len(info.TokensFiles))
		for name := range info.TokensFiles {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Printf("  %s tokens (one per seeded user): %s\n", name, info.TokensFiles[name])
		}
		fmt.Printf("  compose: docker compose -f %s -p %s logs %s\n", info.ComposeFile, info.Project, "app")
		fmt.Printf("  stop:    fixlab env down -project %s\n", dir)
		return 0
	case "down":
		if err := env.Down(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	default:
		failed := 0
		for _, s := range env.Check(ctx) {
			fmt.Printf("%-7s %-12s %s\n", s.Status, s.Name, s.Detail)
			if s.Status == result.Fail {
				failed++
			}
		}
		if failed > 0 {
			fmt.Printf("%s: %d step(s) failed; logs in %s\n", p.Name, failed, env.Dir)
			return 1
		}
		fmt.Printf("%s: profile works\n", p.Name)
		return 0
	}
}
