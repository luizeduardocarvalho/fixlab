package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/luizeduardocarvalho/fixlab/internal/profile"
	"github.com/luizeduardocarvalho/fixlab/internal/result"
)

// stageField is the result field a profile stage reports to, and the name
// failures are recorded under.
func (r *run) stageField(name string) (*string, string) {
	switch name {
	case profile.StageBuild:
		return &r.res.StageBuild, "build"
	case profile.StageVet:
		return &r.res.StageVet, "vet"
	case profile.StageLint:
		return &r.res.StageLint, "lint"
	default:
		return &r.res.StageGoTest, "go_test"
	}
}

// stages runs the profile's stages on the candidate, in order. It reports
// false when a gate stage did not pass, which skips everything after it.
func (r *run) stages(ctx context.Context) bool {
	for _, s := range r.cfg.Profile.Stages {
		r.stage(ctx, s)
		if status, _ := r.stageField(s.Name); s.Gate && *status != result.Pass {
			return false
		}
	}
	return true
}

// stage runs one stage on the candidate and records its status.
func (r *run) stage(ctx context.Context, s profile.Stage) {
	switch {
	case s.Name == profile.StageTest:
		r.testStage(ctx, s)
	case s.Results != "":
		r.findingsStage(ctx, s)
	default:
		r.commandStage(ctx, s)
	}
}

// shell runs a profile command in dir with the toolchain's environment. The
// -go binary's dir goes first on PATH, so "go" in a profile is the Go the
// harness was told to use.
func (r *run) shell(dir, log string, s profile.Stage, command string) cmdSpec {
	keys := make([]string, 0, len(r.cfg.Profile.Toolchain.Env))
	for k := range r.cfg.Profile.Toolchain.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		env = append(env, k+"="+r.cfg.Profile.Toolchain.Env[k])
	}
	if strings.ContainsRune(r.cfg.GoBin, filepath.Separator) {
		env = append(env, "PATH="+filepath.Dir(r.cfg.GoBin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return cmdSpec{dir: dir, log: log, timeout: s.TimeoutDuration(), name: "sh", args: []string{"-c", command}, env: env}
}

// runStage runs a stage's command in dir, once per item when it has Each,
// and returns the concatenated stdout and the first error.
func (r *run) runStage(ctx context.Context, s profile.Stage, dir, log string) ([]byte, error) {
	if s.Each == "" {
		return r.exec(ctx, r.shell(dir, log, s, s.Run))
	}
	list, err := r.exec(ctx, r.shell(dir, log+"-list", s, s.Each))
	if err != nil {
		return list, err
	}
	var out bytes.Buffer
	var firstErr error
	for _, item := range strings.Fields(string(list)) {
		o, err := r.exec(ctx, r.shell(dir, log, s, strings.ReplaceAll(s.Run, "{item}", item)))
		out.Write(o)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return out.Bytes(), firstErr
}

// commandStage is a stage its exit code decides.
func (r *run) commandStage(ctx context.Context, s profile.Stage) {
	status, name := r.stageField(s.Name)
	if _, err := r.runStage(ctx, s, r.candWT, "stage-"+s.Name); err != nil {
		if decl := r.hiddenCollision(err.Error()); decl != "" {
			// The candidate's own tests declare a name the hidden tests also
			// declare. It couldn't know the name; the task must rename it.
			r.infraError(status, name, fmt.Errorf("hidden test declaration %q collides with the candidate's code; rename it in the task's hidden tests: %w", decl, err))
		} else {
			r.fail(status, name, err)
		}
		return
	}
	*status = result.Pass
}

// findingsStage is a stage judged by the findings its reader parses. With
// compare: baseline it fails only on findings the candidate introduced (by
// file, check and message; line numbers ignored), since the codebase may
// have pre-existing ones. Findings in hidden test files are excluded: the
// candidate didn't write them.
func (r *run) findingsStage(ctx context.Context, s profile.Stage) {
	status, name := r.stageField(s.Name)
	base := map[string]int{}
	if s.Compare == "baseline" {
		var err error
		if base, err = r.findings(ctx, s, r.baseWT, "stage-"+s.Name+"-baseline"); err != nil {
			r.infraError(status, name, err)
			return
		}
	}
	cand, err := r.findings(ctx, s, r.candWT, "stage-"+s.Name+"-candidate")
	if err != nil {
		r.infraError(status, name, err)
		return
	}
	hidden := map[string]bool{}
	for _, h := range r.task.HiddenTests {
		hidden[filepath.Clean(h.To)] = true
	}
	var added []string
	for key, n := range cand {
		if hidden[strings.SplitN(key, "|", 2)[0]] {
			continue
		}
		for i := base[key]; i < n; i++ {
			added = append(added, key)
		}
	}
	sort.Strings(added)
	r.res.LintNewFindings = len(added)
	if len(added) > 0 {
		r.fail(status, name, fmt.Errorf("%d new %s finding(s): %s", len(added), s.Name, strings.Join(added, "; ")))
		return
	}
	*status = result.Pass
}

type staticcheckFinding struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Location struct {
		File string `json:"file"`
	} `json:"location"`
}

// findings runs a findings stage in dir and counts its findings by
// "file|check|message". The tool exits non-zero when it reports findings;
// only a non-zero exit with nothing parseable is a tool failure.
func (r *run) findings(ctx context.Context, s profile.Stage, dir, log string) (map[string]int, error) {
	out, runErr := r.runStage(ctx, s, dir, log)
	findings := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	parsed := 0
	for sc.Scan() {
		var f staticcheckFinding
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue
		}
		parsed++
		rel, err := filepath.Rel(dir, f.Location.File)
		if err != nil {
			rel = f.Location.File
		}
		findings[rel+"|"+f.Code+"|"+f.Message]++
	}
	if runErr != nil && parsed == 0 {
		return nil, runErr
	}
	return findings, nil
}

// testStage runs the full suite (hidden tests included) on the candidate and
// grades the hidden tests from its results.
func (r *run) testStage(ctx context.Context, s profile.Stage) {
	out, err := r.runStage(ctx, s, r.candWT, "stage-test")
	if writeErr := os.WriteFile(filepath.Join(r.artifacts, "go-test.json"), out, 0o644); writeErr != nil {
		r.infraError(&r.res.StageGoTest, "go_test", writeErr)
		return
	}
	sum := parseGoTest(out)
	r.res.GoTestsTotal, r.res.GoTestsPassed, r.res.GoTestsFailed, r.res.GoTestsSkipped = sum.total, sum.passed, sum.failed, sum.skipped
	// The failure summary below replaces err's output, so host failures are
	// looked for in the test output too.
	infra := testInfraFailure(out, append(slices.Clone(r.cfg.Profile.Tests.InfraErrors), hostFailures...))
	if err != nil {
		if len(sum.failedNames) > 0 {
			err = fmt.Errorf("%d failing: %s", len(sum.failedNames), strings.Join(sum.failedNames, ", "))
		}
		if infra != "" {
			r.infraError(&r.res.StageGoTest, "go_test", fmt.Errorf("test infrastructure failed (%s): %w", infra, err))
		} else {
			r.fail(&r.res.StageGoTest, "go_test", err)
		}
	} else {
		r.res.StageGoTest = result.Pass
	}

	names, nameErr := r.task.HiddenGoTests()
	if nameErr != nil {
		r.infraError(&r.res.StageHiddenTests, "hidden_tests", nameErr)
		return
	}
	r.res.HiddenTestsTotal = len(names)
	g := gradeHidden(names, sum.topLevel, infra)
	r.res.HiddenTestsPassed, r.res.HiddenTestsFailed = g.passed, g.failed
	switch g.status {
	case result.Pass:
		r.res.StageHiddenTests = result.Pass
	case result.Error:
		r.infraError(&r.res.StageHiddenTests, "hidden_tests", g.err)
	default:
		r.fail(&r.res.StageHiddenTests, "hidden_tests", g.err)
	}
}

// goTestEvent is one line of `go test -json`.
type goTestEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
}

type goTestSummary struct {
	total, passed, failed, skipped int
	// topLevel maps a top-level test name to its final action.
	topLevel map[string]string
	// failedNames lists failed tests and packages that failed to build or
	// failed outside any test.
	failedNames []string
}

func parseGoTest(out []byte) goTestSummary {
	s := goTestSummary{topLevel: map[string]string{}}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var ev goTestEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Test == "" {
			if ev.Action == "fail" {
				s.failedNames = append(s.failedNames, "package "+ev.Package)
			}
			continue
		}
		switch ev.Action {
		case "pass":
			s.passed++
		case "fail":
			s.failed++
			s.failedNames = append(s.failedNames, ev.Test)
		case "skip":
			s.skipped++
		default:
			continue
		}
		s.total++
		if !strings.Contains(ev.Test, "/") {
			s.topLevel[ev.Test] = ev.Action
		}
	}
	return s
}

var redeclared = regexp.MustCompile(`(\w+) redeclared in this block`)

// hiddenCollision returns the first identifier that compiler output reports
// as redeclared and that a hidden test file declares at top level, or "".
func (r *run) hiddenCollision(out string) string {
	decls := map[string]bool{}
	for _, h := range r.task.HiddenTests {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(r.task.Dir, h.From), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					decls[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						decls[s.Name.Name] = true
					case *ast.ValueSpec:
						for _, n := range s.Names {
							decls[n.Name] = true
						}
					}
				}
			}
		}
	}
	for _, m := range redeclared.FindAllStringSubmatch(out, -1) {
		if decls[m[1]] {
			return m[1]
		}
	}
	return ""
}

type hiddenGrade struct {
	passed, failed int
	status         string // result.Pass, result.Fail or result.Error
	err            error
}

// gradeHidden scores the hidden tests from their top-level go test actions.
// infra is the test-infrastructure marker found in the output, if any.
func gradeHidden(names []string, topLevel map[string]string, infra string) hiddenGrade {
	var g hiddenGrade
	var missing, skipped []string
	for _, n := range names {
		switch topLevel[n] {
		case "pass":
			g.passed++
		case "skip":
			// DB-backed tests skip without Docker: a skipped hidden test was
			// never graded, so it is an infra error, not a failure.
			skipped = append(skipped, n)
			g.failed++
		case "fail":
			g.failed++
		default:
			missing = append(missing, n)
			g.failed++
		}
	}
	switch {
	case len(names) == 0:
		g.status, g.err = result.Error, fmt.Errorf("no Test functions found in hidden test files")
	case len(skipped) > 0:
		g.status, g.err = result.Error, fmt.Errorf("%d/%d hidden tests skipped (is Docker running?); not a verdict: %v", len(skipped), len(names), skipped)
	case g.failed > 0 && infra != "":
		g.status, g.err = result.Error, fmt.Errorf("test infrastructure failed (%s); hidden test results are not a verdict", infra)
	case g.failed > 0:
		g.status, g.err = result.Fail, fmt.Errorf("%d/%d hidden tests did not pass (not run: %v)", g.failed, len(names), missing)
	default:
		g.status = result.Pass
	}
	return g
}

// testInfraFailure returns the first of the profile's infra markers found in
// the output of a go test -json run, or "". A heuristic: it lets a dead test
// database be reported as an infra error instead of counting against the
// candidate.
func testInfraFailure(out []byte, markers []string) string {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var ev struct{ Action, Output string }
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Action != "output" {
			continue
		}
		for _, m := range markers {
			if strings.Contains(ev.Output, m) {
				return m
			}
		}
	}
	return ""
}
