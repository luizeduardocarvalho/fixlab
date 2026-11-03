package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luizeduardocarvalho/fixlab/internal/profile"
	"github.com/luizeduardocarvalho/fixlab/internal/result"
	pkgstack "github.com/luizeduardocarvalho/fixlab/internal/stack"
)

// Env is a project's stack run for direct use: by a person, or by an agent
// reproducing a bug end to end. The app is built from the project dir as it
// is, uncommitted changes included, and published on 127.0.0.1.
type Env struct {
	Profile *profile.Profile
	Project string // project dir the app is built from
	Dir     string // state: rendered compose file, module assets, logs
	GoBin   string // go binary for stage commands, as for runs
	Logf    func(format string, args ...any)
}

// EnvInfo is how to reach a running env; Up also writes it to Dir/env.json.
type EnvInfo struct {
	URL         string            `json:"url"`
	TokensFiles map[string]string `json:"tokens_files,omitempty"` // issuer service → tokens.json on the host
	Project     string            `json:"compose_project"`
	ComposeFile string            `json:"compose_file"`
}

// EnvRoot is where envs keep their state, one dir per EnvID: in the user's
// cache dir, since an env belongs to a project checkout, not to a workspace.
func EnvRoot() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "fixlab", "env"), nil
}

// EnvID names a project's env: the profile name plus a hash of the project
// dir, so two checkouts of one project (a worktree, a copy) get separate
// stacks and state.
func EnvID(p *profile.Profile, projectDir string) string {
	sum := sha256.Sum256([]byte(projectDir))
	return p.Name + "-" + hex.EncodeToString(sum[:])[:8]
}

func (e Env) composeProject() string { return ProjectPrefix + "env-" + EnvID(e.Profile, e.Project) }

func (e Env) composeFile() string { return filepath.Join(e.Dir, "compose.yml") }

func (e Env) compose(log string, timeout time.Duration, args ...string) cmdSpec {
	return cmdSpec{log: log, timeout: timeout, name: "docker",
		args: append([]string{"compose", "-f", e.composeFile(), "-p", e.composeProject()}, args...)}
}

func (e Env) exec(ctx context.Context, spec cmdSpec) ([]byte, error) {
	return execCmd(ctx, filepath.Join(e.Dir, "logs"), spec)
}

func (e Env) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

// Up replaces any env of this profile with a fresh one: stack up, seeded.
func (e Env) Up(ctx context.Context) (EnvInfo, error) {
	if err := e.reset(ctx); err != nil {
		return EnvInfo{}, err
	}
	return e.up(ctx)
}

// reset removes a previous env of this profile and everything it left in Dir.
func (e Env) reset(ctx context.Context) error {
	if err := e.Down(ctx); err != nil {
		return err
	}
	return os.RemoveAll(e.Dir)
}

func (e Env) up(ctx context.Context) (EnvInfo, error) {
	var info EnvInfo
	p := e.Profile
	if p.App == nil {
		return info, fmt.Errorf("profile %s has no app to run", p.Name)
	}
	assets, out := filepath.Join(e.Dir, "assets"), filepath.Join(e.Dir, "out")
	for _, d := range []string{assets, out} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return info, err
		}
	}
	outs, err := pkgstack.Prepare(p, assets)
	if err != nil {
		return info, err
	}
	file, err := pkgstack.Compose(pkgstack.Spec{Profile: p, Worktree: e.Project, Assets: assets, Out: out, PublishApp: true}, outs)
	if err != nil {
		return info, err
	}
	if err := os.WriteFile(e.composeFile(), file, 0o644); err != nil {
		return info, err
	}

	e.logf("building and starting %s (logs: %s)", e.composeProject(), filepath.Join(e.Dir, "logs"))
	up := append([]string{"up", "-d", "--build", "--wait", "--wait-timeout", "600"}, pkgstack.InfraServices(p)...)
	if _, err := e.exec(ctx, e.compose("compose-up", 15*time.Minute, up...)); err != nil {
		return info, fmt.Errorf("stack up: %w", err)
	}
	if p.Seed != nil {
		sql, err := os.ReadFile(filepath.Join(p.Dir, p.Seed.SQL))
		if err != nil {
			return info, err
		}
		e.logf("seeding %s", p.Seed.Into)
		if _, err := e.exec(ctx, withStdin(e.compose("seed", 5*time.Minute, "run", "--rm", "-T", pkgstack.SeedService), string(sql))); err != nil {
			return info, fmt.Errorf("seed: %w", err)
		}
	}
	addr, err := e.exec(ctx, e.compose("compose-port", time.Minute, "port", pkgstack.AppService, fmt.Sprint(p.App.Port)))
	if err != nil {
		return info, err
	}
	info = EnvInfo{
		URL:         "http://" + strings.TrimSpace(string(addr)),
		TokensFiles: map[string]string{},
		Project:     e.composeProject(),
		ComposeFile: e.composeFile(),
	}
	for name, s := range p.Services {
		if s.Module == profile.ModuleJWTIssuer {
			info.TokensFiles[name] = filepath.Join(assets, name, "tokens.json")
		}
	}
	return info, writeJSON(filepath.Join(e.Dir, "env.json"), info)
}

// Down removes the env's containers, volumes and built image, if any, and
// its state except the logs.
func (e Env) Down(ctx context.Context) error {
	if _, err := os.Stat(e.composeFile()); err != nil {
		return nil
	}
	if _, err := e.exec(ctx, e.compose("compose-down", 3*time.Minute, "down", "-v", "--remove-orphans", "--rmi", "local", "--timeout", "5")); err != nil {
		return err
	}
	for _, name := range []string{"compose.yml", "env.json", "assets", "out"} {
		if err := os.RemoveAll(filepath.Join(e.Dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// CheckStep is one line of an env check.
type CheckStep struct {
	Name   string
	Status string // result.Pass, result.Fail or result.Skipped
	Detail string
}

// Check proves the profile works on the project as it is: every stage
// passes (findings are reported, not judged: there is no baseline), the
// stack comes up healthy and seeds, and one load round makes requests with
// no 5xx and every check passing. The env is removed afterwards.
func (e Env) Check(ctx context.Context) []CheckStep {
	var steps []CheckStep
	add := func(name, status, detail string) { steps = append(steps, CheckStep{name, status, detail}) }

	if err := e.reset(ctx); err != nil {
		add("reset", result.Fail, err.Error())
		return steps
	}
	if !e.checkStages(ctx, add) {
		return steps
	}
	if e.Profile.App == nil {
		add("stack", result.Skipped, "the profile has no app")
		return steps
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := e.Down(cctx); err != nil {
			e.logf("env down: %v", err)
		}
	}()
	info, err := e.up(ctx)
	if err != nil {
		add("stack", result.Fail, err.Error())
		return steps
	}
	add("stack", result.Pass, "healthy at "+info.URL+seededNote(e.Profile))
	if e.Profile.Load == nil {
		add("load", result.Skipped, "the profile has no load script")
		return steps
	}
	s, err := e.loadRound(ctx)
	switch {
	case err != nil:
		add("load", result.Fail, err.Error())
	case s.ErrorRate > 0 || s.Checks < 1:
		add("load", result.Fail, fmt.Sprintf("%d requests, 5xx rate %.4f, checks passed %.4f: the unchanged project must pass its own load script", s.Requests, s.ErrorRate, s.Checks))
	default:
		add("load", result.Pass, fmt.Sprintf("%d requests, p95 %.1fms, no 5xx, every check passed", s.Requests, s.P95Ms))
	}
	return steps
}

func seededNote(p *profile.Profile) string {
	if p.Seed == nil {
		return ""
	}
	return ", seeded"
}

// checkStages runs the profile's stages in the project dir with the same
// code that grades candidates. It reports false when a gate stage failed.
func (e Env) checkStages(ctx context.Context, add func(name, status, detail string)) bool {
	p := *e.Profile
	p.Stages = make([]profile.Stage, len(e.Profile.Stages))
	for i, s := range e.Profile.Stages {
		s.Compare = "" // no baseline: count findings instead of judging them
		p.Stages[i] = s
	}
	// The run logs nothing: its messages assume a candidate graded against a
	// baseline and hidden tests. The step table says what happened.
	r := &run{
		cfg:       Config{Profile: &p, GoBin: e.GoBin},
		id:        "env-check",
		candWT:    e.Project,
		artifacts: filepath.Join(e.Dir, "check"),
		res:       result.New(),
	}
	if err := os.MkdirAll(r.artifacts, 0o755); err != nil {
		add("stages", result.Fail, err.Error())
		return false
	}
	for _, s := range p.Stages {
		e.logf("stage %s: %s", s.Name, s.Run)
		r.res.FailureStage, r.res.Error = "", ""
		r.stage(ctx, s)
		status, _ := r.stageField(s.Name)
		detail := r.res.Error
		switch {
		case s.Name == profile.StageTest:
			detail = fmt.Sprintf("%d tests: %d passed, %d failed, %d skipped", r.res.GoTestsTotal, r.res.GoTestsPassed, r.res.GoTestsFailed, r.res.GoTestsSkipped)
			if r.res.GoTestsSkipped > 0 && *status == result.Pass {
				detail += " (skipped tests can't grade hidden tests; is Docker running?)"
			}
			if *status != result.Pass {
				detail += ": " + r.res.Error
			}
		case s.Results != "" && *status != result.Error:
			// Pre-existing findings are expected; runs only fail on new ones.
			detail = fmt.Sprintf("%d findings on the project as it is (runs fail only on findings a candidate adds)", r.res.LintNewFindings)
			*status = result.Pass
		}
		if *status == result.Error {
			*status = result.Fail
		}
		add("stage "+s.Name, *status, detail)
		if s.Gate && *status != result.Pass {
			return false
		}
	}
	return true
}

// loadRound runs the load script once against the env.
func (e Env) loadRound(ctx context.Context) (loadSummary, error) {
	l := e.Profile.Load
	out := filepath.Join(e.Dir, "out")
	if _, err := e.exec(ctx, e.compose("k6", 10*time.Minute, "run", "--rm", "-T",
		"-e", fmt.Sprintf("VUS=%d", l.VUs), "-e", "DURATION="+l.Duration,
		pkgstack.LoadService, "run", "--quiet", pkgstack.LoadScript(l.Script))); err != nil {
		return loadSummary{}, err
	}
	return readLoadSummary(filepath.Join(out, "summary.json"))
}

// ReadEnvInfo reads what Up recorded about a running env.
func (e Env) ReadEnvInfo() (EnvInfo, error) {
	var info EnvInfo
	b, err := os.ReadFile(filepath.Join(e.Dir, "env.json"))
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(b, &info)
}
