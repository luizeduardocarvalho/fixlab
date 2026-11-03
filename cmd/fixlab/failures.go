package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// failuresCmd lists every attempt on the current tests that didn't fully
// pass, with the hidden tests it missed, then each task's hardest tests.
func failuresCmd(args []string) int {
	fs := flag.NewFlagSet("failures", flag.ExitOnError)
	var c common
	c.register(fs)
	dir := fs.String("history", "", "history repo checkout (default: env "+historyEnv+")")
	taskID := fs.String("task", "", "only this task id")
	variant := fs.String("variant", "", "only this variant (experiment or strategy name)")
	_ = fs.Parse(args)

	cfg, err := c.config()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	root := resolveHistory(*dir)
	if root == "" {
		fmt.Fprintln(os.Stderr, "no history dir: pass -history or set "+historyEnv)
		return 2
	}
	d, err := buildData(cfg, root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	shown := 0
	for _, f := range d.Failures {
		if (*taskID != "" && f.TaskID != *taskID) || (*variant != "" && f.Variant != *variant) {
			continue
		}
		shown++
		verdict := "fail at " + f.Stage
		if f.Infra {
			verdict = fmt.Sprintf("infra error at %s (after %d retries; not a verdict)", f.Stage, f.InfraRetries)
		}
		fmt.Printf("%s  %s  %s  %s  hidden %d/%d\n", f.TaskID, f.Variant, f.RunID[len(f.RunID)-6:], verdict, f.HiddenPassed, f.HiddenTotal)
		byKind := map[string][]string{}
		var kinds []string
		for _, m := range f.Missed {
			k := m.Kind
			if k == "" {
				k = "other"
			}
			if _, ok := byKind[k]; !ok {
				kinds = append(kinds, k)
			}
			name := m.Test
			if m.Outcome != "fail" {
				name += " (" + m.Outcome + ")"
			}
			byKind[k] = append(byKind[k], name)
		}
		for _, k := range kinds {
			fmt.Printf("    %-12s %s\n", k+":", strings.Join(byKind[k], ", "))
		}
		if (f.Infra || len(f.Missed) == 0) && f.Error != "" {
			fmt.Printf("    error:       %s\n", f.Error)
		}
		fmt.Printf("    files:       %s/ (candidate.diff, transcript.jsonl, go-test.json)\n", filepath.Join(root, f.Dir))
	}
	if shown == 0 {
		fmt.Println("Every attempt on the current tests passed.")
	}

	for _, h := range d.Hardest {
		if *taskID != "" && h.TaskID != *taskID {
			continue
		}
		fmt.Printf("\nHardest hidden tests in %s (%d agent attempts on the current tests, infra errors left out):\n", h.TaskID, h.Attempts)
		for _, t := range h.Tests {
			if t.Failed == 0 {
				continue
			}
			fmt.Printf("  failed %2d/%-2d  %-12s %s\n", t.Failed, h.Attempts, t.Kind, t.Test)
		}
	}
	return 0
}
