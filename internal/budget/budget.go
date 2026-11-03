// Package budget caps how many agent attempts a task gets, so neither a
// benchmark nor the search for a reference fix turns into rerunning until
// something passes.
//
// Attempts are grouped by Inputs: what an attempt sees (the rendered prompt,
// which carries the issue or spec) and how it is graded (base commit and
// hidden tests). Benchmark arms are measurements: each experiment gets
// AttemptsPerRound attempts per Inputs. The reference search
// ("reference-search") runs in rounds of AttemptsPerRound attempts. A second
// round needs new Inputs, meaning the spec or tests changed after triage, and
// there are at most MaxRounds. Past that, a person decides what happens next.
//
// Only agent attempts that reached a verdict count. Infrastructure errors,
// regrades (which grade an old attempt's diff again) and attempts recorded
// before grading_sha256 existed don't.
package budget

import (
	"fmt"
	"sort"

	"github.com/luizeduardocarvalho/fixlab/internal/result"
)

const (
	ReferenceSearch  = "reference-search"
	AttemptsPerRound = 3
	MaxRounds        = 2
)

// Inputs identifies what a round of attempts faces.
type Inputs struct {
	Prompt  string // sha256 of the rendered prompt (result.PromptSHA256)
	Grading string // task.Task.GradingSHA256
}

func (in Inputs) short() string {
	return shorten(in.Prompt) + "/" + shorten(in.Grading)
}

// Check reports whether runs new agent attempts may start for a task with
// inputs under experiment, given the task's earlier attempts. The error says
// why not and what to do instead.
func Check(prior []result.Result, experiment string, inputs Inputs, runs int) error {
	if runs > AttemptsPerRound {
		return fmt.Errorf("-runs %d is more than the %d attempts a round gets", runs, AttemptsPerRound)
	}

	var current []result.Result
	rounds := map[Inputs]bool{}
	for _, r := range prior {
		if r.ImplementerKind != "agent" || r.RegradedFrom != "" || r.GradingSHA256 == "" ||
			r.Experiment != experiment || r.HasInfraError() {
			continue
		}
		in := Inputs{Prompt: r.PromptSHA256, Grading: r.GradingSHA256}
		rounds[in] = true
		if in == inputs {
			current = append(current, r)
		}
	}

	if experiment != ReferenceSearch {
		if len(current)+runs > AttemptsPerRound {
			return fmt.Errorf("experiment %q already has %d of its %d attempts with these inputs; "+
				"rerunning a measurement until it passes would bias it (regrade or compare instead)",
				experiment, len(current), AttemptsPerRound)
		}
		return nil
	}

	for _, r := range current {
		if r.Pass {
			return fmt.Errorf("attempt %s already passed with these inputs; promote it instead of searching further", r.RunID)
		}
	}
	if len(current)+runs > AttemptsPerRound {
		return fmt.Errorf("this round already used %d of its %d attempts with these inputs; "+
			"triage the failures, and start round %d only after fixing the spec or tests",
			len(current), AttemptsPerRound, len(rounds)+1)
	}
	if len(current) == 0 && len(rounds) >= MaxRounds {
		return fmt.Errorf("the reference search already ran %d rounds (prompt/grading %s) without a pass; "+
			"stop and decide with the maintainer: split the issue, write the fix by hand, or drop the task",
			len(rounds), sortedKeys(rounds))
	}
	return nil
}

func shorten(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func sortedKeys(set map[Inputs]bool) string {
	out := make([]string, 0, len(set))
	for in := range set {
		out = append(out, in.short())
	}
	sort.Strings(out)
	return fmt.Sprint(out)
}
