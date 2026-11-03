package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/luizeduardocarvalho/fixlab/internal/report"
	"github.com/luizeduardocarvalho/fixlab/internal/result"
)

// historyCmd summarises recorded attempts: pass rate per task and variant,
// excluding infrastructure errors, plus mean cost, time and diff size.
func historyCmd(args []string) int {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	dir := fs.String("history", "", "history repo checkout (default: env "+historyEnv+")")
	asCSV := fs.Bool("csv", false, "print every attempt as one CSV row instead of the summary")
	task := fs.String("task", "", "only this task id")
	_ = fs.Parse(args)

	root := resolveHistory(*dir)
	if root == "" {
		fmt.Fprintln(os.Stderr, "no history dir: pass -history or set "+historyEnv)
		return 2
	}
	rows, err := report.LoadAttempts(root, *task)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asCSV {
		return writeCSV(rows)
	}
	printSummary(report.Latest(rows))
	return 0
}

type group struct {
	n, pass, fail, infra                 int
	cost, agentSec, lines                float64
	costN, outside, outsideN, deniedRefs int
}

func printSummary(rows []result.Result) {
	groups := map[string]*group{}
	var keys []string
	for _, r := range rows {
		// Task version is part of the key: attempts graded by different hidden
		// tests are never pooled.
		key := strings.Join([]string{r.TaskID, short(r.TaskSHA256), orDash(r.Experiment), r.Implementer, orDash(r.PromptVariant), orDash(r.ClaudeMDVariant), orDash(r.Effort), orDash(r.AgentPlugins)}, "\t")
		g, ok := groups[key]
		if !ok {
			g = &group{}
			groups[key] = g
			keys = append(keys, key)
		}
		g.n++
		switch {
		case r.Pass:
			g.pass++
		case r.HasInfraError():
			g.infra++
		default:
			g.fail++
		}
		g.lines += float64(r.DiffLinesAdded + r.DiffLinesRemoved)
		if r.AgentCostUSD != nil {
			g.cost += *r.AgentCostUSD
			g.costN++
		}
		if r.AgentDurationSec != nil {
			g.agentSec += *r.AgentDurationSec
		}
		if r.DiffFilesOutsideReference != nil {
			g.outside += *r.DiffFilesOutsideReference
			g.outsideN++
		}
		if r.AgentDeniedPathRefs != nil {
			g.deniedRefs += *r.AgentDeniedPathRefs
		}
	}
	sort.Strings(keys)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TASK\tTASK VERSION\tEXPERIMENT\tIMPLEMENTER\tPROMPT\tCLAUDE.MD\tEFFORT\tPLUGINS\tATTEMPTS\tPASS\tFAIL\tINFRA\tPASS RATE\tAVG LIST COST\tAVG AGENT TIME\tAVG DIFF LINES\tAVG FILES OUTSIDE REF\tDENIED-PATH REFS")
	for _, k := range keys {
		g := groups[k]
		rate := "-"
		if graded := g.pass + g.fail; graded > 0 {
			rate = fmt.Sprintf("%.0f%% (%d/%d)", 100*float64(g.pass)/float64(graded), g.pass, graded)
		}
		cost, agentTime, outside := "-", "-", "-"
		if g.costN > 0 {
			cost = fmt.Sprintf("$%.2f", g.cost/float64(g.costN))
			agentTime = fmt.Sprintf("%.0fs", g.agentSec/float64(g.costN))
		}
		if g.outsideN > 0 {
			outside = fmt.Sprintf("%.1f", float64(g.outside)/float64(g.outsideN))
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%.0f\t%s\t%d\n",
			k, g.n, g.pass, g.fail, g.infra, rate, cost, agentTime, g.lines/float64(g.n), outside, g.deniedRefs)
	}
	w.Flush()
}

// writeCSV prints all attempts with one column per result field, in schema order.
func writeCSV(rows []result.Result) int {
	t := reflect.TypeOf(result.Result{})
	var header []string
	for i := 0; i < t.NumField(); i++ {
		header = append(header, strings.Split(t.Field(i).Tag.Get("json"), ",")[0])
	}
	w := csv.NewWriter(os.Stdout)
	_ = w.Write(header)
	for _, r := range rows {
		v := reflect.ValueOf(r)
		rec := make([]string, v.NumField())
		for i := range rec {
			f := v.Field(i)
			if f.Kind() == reflect.Pointer {
				if f.IsNil() {
					continue
				}
				f = f.Elem()
			}
			rec[i] = fmt.Sprint(f.Interface())
		}
		_ = w.Write(rec)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return orDash(sha)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
