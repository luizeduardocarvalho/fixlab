package report

import (
	"fmt"
	"math"
)

// Comparison is a challenger strategy measured against the default on the
// benchmark tasks, with the adoption rule applied.
type Comparison struct {
	Candidate string        `json:"candidate"`
	Default   string        `json:"default"`
	Tasks     []TaskCompare `json:"tasks"`
	Verdict   string        `json:"verdict"` // adopt, keep-default, not-enough-data
	Reasons   []string      `json:"reasons"`
}

// TaskCompare is both strategies on one task.
type TaskCompare struct {
	TaskID    string `json:"task_id"`
	Default   Stats  `json:"default"`
	Candidate Stats  `json:"candidate"`
}

// Stats summarise a strategy's agent attempts on a task's current tests,
// infra errors left out.
type Stats struct {
	Runs         int     `json:"runs"`
	Passes       int     `json:"passes"`
	HiddenShare  float64 `json:"hidden_share"` // hidden tests passed / hidden tests run
	SecurityRuns int     `json:"security_runs"`
	SecurityPass int     `json:"security_pass"` // runs passing every security test
	TokensAvg    float64 `json:"tokens_avg"`
	AgentSecAvg  float64 `json:"agent_sec_avg"`
	HasSecurity  bool    `json:"has_security"`
	PassRate     float64 `json:"pass_rate"`
	SecurityRate float64 `json:"security_rate"`
}

// CompareRule mirrors bench.Rule, so report doesn't depend on bench.
type CompareRule struct {
	MinRuns          int
	MaxTokenIncrease float64
}

// Compare measures candidate against def on tasks (task ids) and applies the
// rule: on every task, at least min runs each, a pass rate and a security
// pass rate no lower than the default's, and average tokens within the
// allowed increase unless some task's pass rate went up.
func Compare(d Data, candidate, def string, tasks []string, rule CompareRule) Comparison {
	c := Comparison{Candidate: candidate, Default: def, Tasks: []TaskCompare{}, Reasons: []string{}}
	var defTok, candTok float64
	improved, short := false, false
	for _, id := range tasks {
		tc := TaskCompare{TaskID: id, Default: statsFor(d, id, def), Candidate: statsFor(d, id, candidate)}
		c.Tasks = append(c.Tasks, tc)
		if tc.Default.Runs < rule.MinRuns || tc.Candidate.Runs < rule.MinRuns {
			short = true
			c.Reasons = append(c.Reasons, fmt.Sprintf("%s: %d default and %d %s runs, %d each needed", id, tc.Default.Runs, tc.Candidate.Runs, candidate, rule.MinRuns))
			continue
		}
		if tc.Candidate.PassRate < tc.Default.PassRate {
			c.Reasons = append(c.Reasons, fmt.Sprintf("%s: pass rate %s, below the default's %s", id, pct(tc.Candidate.PassRate), pct(tc.Default.PassRate)))
		}
		if tc.Candidate.PassRate > tc.Default.PassRate {
			improved = true
		}
		if tc.Default.HasSecurity && tc.Candidate.SecurityRate < tc.Default.SecurityRate {
			c.Reasons = append(c.Reasons, fmt.Sprintf("%s: security tests passed in %s of runs, below the default's %s", id, pct(tc.Candidate.SecurityRate), pct(tc.Default.SecurityRate)))
		}
		defTok += tc.Default.TokensAvg
		candTok += tc.Candidate.TokensAvg
	}
	switch {
	case short:
		c.Verdict = "not-enough-data"
		return c
	case defTok > 0 && candTok > defTok*(1+rule.MaxTokenIncrease) && !improved:
		c.Reasons = append(c.Reasons, fmt.Sprintf("uses %s more tokens per run with no pass-rate gain (%s allowed)", pct(candTok/defTok-1), pct(rule.MaxTokenIncrease)))
	}
	if len(c.Reasons) == 0 {
		c.Verdict = "adopt"
		c.Reasons = append(c.Reasons, "no worse on any task, and within the token allowance or better somewhere")
	} else {
		c.Verdict = "keep-default"
	}
	return c
}

func statsFor(d Data, taskID, variant string) Stats {
	var s Stats
	var hiddenPassed, hiddenRun int
	var tokens, secs float64
	var tokenRuns int
	security := map[string]bool{}
	for _, t := range d.Tasks {
		if t.ID == taskID {
			for _, k := range t.TestKinds {
				if k.Kind == "security" {
					for _, name := range k.Tests {
						security[name] = true
					}
				}
			}
		}
	}
	s.HasSecurity = len(security) > 0
	outcomes := map[string]map[string]string{}
	for _, o := range d.Tests {
		if outcomes[o.RunID] == nil {
			outcomes[o.RunID] = map[string]string{}
		}
		outcomes[o.RunID][o.Test] = o.Outcome
	}
	for _, a := range d.Attempts {
		if a.TaskID != taskID || !a.Agent || !a.Current || a.Infra || a.Variant != variant {
			continue
		}
		s.Runs++
		if a.Pass {
			s.Passes++
		}
		hiddenPassed += a.HiddenPassed
		hiddenRun += a.HiddenTotal
		if got := outcomes[a.RunID]; got != nil && s.HasSecurity {
			s.SecurityRuns++
			ok := true
			for name := range security {
				if got[name] != "pass" {
					ok = false
				}
			}
			if ok {
				s.SecurityPass++
			}
		}
		if a.InputTok != nil {
			tokens += float64(*a.InputTok + *a.OutputTok + *a.CacheReadTok + *a.CacheWrite)
			tokenRuns++
		}
		if a.AgentSec != nil {
			secs += *a.AgentSec
		}
	}
	if s.Runs > 0 {
		s.PassRate = float64(s.Passes) / float64(s.Runs)
		s.AgentSecAvg = secs / float64(s.Runs)
	}
	if hiddenRun > 0 {
		s.HiddenShare = float64(hiddenPassed) / float64(hiddenRun)
	}
	if s.SecurityRuns > 0 {
		s.SecurityRate = float64(s.SecurityPass) / float64(s.SecurityRuns)
	}
	if tokenRuns > 0 {
		s.TokensAvg = tokens / float64(tokenRuns)
	}
	return s
}

func pct(v float64) string { return fmt.Sprintf("%.0f%%", math.Round(v*100)) }
