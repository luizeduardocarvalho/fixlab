// Package profile reads a project's eval profile: everything the harness
// needs to know about one project to build, test, run and load-test it.
//
// A profile is a directory, .eval/ at the project root, holding profile.yaml
// and the files it names (Dockerfile, seed, load script, overlay). The core
// harness knows nothing project-specific beyond what a profile declares.
package profile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

// Dir is where a project keeps its profile, relative to the project root.
const Dir = ".eval"

// Module names a service can use.
const (
	ModulePostgres  = "postgres"
	ModuleJWTIssuer = "jwt-issuer"
)

// Stage names. Each maps to fixed fields of the result record.
const (
	StageBuild = "build"
	StageVet   = "vet"
	StageLint  = "lint"
	StageTest  = "test"
)

// Result readers.
const (
	ReaderGoJSON          = "go-json"
	ReaderStaticcheckJSON = "staticcheck-json"
)

type Profile struct {
	Name      string             `yaml:"name"`
	Toolchain Toolchain          `yaml:"toolchain"`
	App       *App               `yaml:"app"`
	Services  map[string]Service `yaml:"services"`
	Seed      *Seed              `yaml:"seed"`
	Load      *Load              `yaml:"load"`
	Stages    []Stage            `yaml:"stages"`
	Tests     Tests              `yaml:"tests"`
	Agent     Agent              `yaml:"agent"`

	// Dir is the absolute directory holding profile.yaml; every path in the
	// profile is relative to it.
	Dir string `yaml:"-"`
	// SHA256 hashes every file under Dir, so a changed profile shows up in
	// the history like a changed task.
	SHA256 string `yaml:"-"`
}

// Toolchain is how stage commands run on the host.
type Toolchain struct {
	Env map[string]string `yaml:"env"` // added to every stage command's environment
}

// App is the project's server, built from the worktree under test.
type App struct {
	// Dockerfile is built with the worktree as context and the profile dir
	// as the extra build context "profile", so it can add overlay files
	// (COPY --from=profile …) without touching the worktree.
	Dockerfile string `yaml:"dockerfile"`
	Port       int    `yaml:"port"`
	Health     string `yaml:"health"` // HTTP path that answers 2xx once the app is ready; probed with wget inside the app container
	// Env values are templates over service outputs, e.g.
	// "{{ .postgres.url }}"; see Outputs for what each module provides.
	Env map[string]string `yaml:"env"`
}

// Service is one instance of a built-in module. Which fields apply depends on
// the module.
type Service struct {
	Module  string `yaml:"module"`
	Version string `yaml:"version"` // postgres: image tag, e.g. 16.4-alpine3.20
	Preset  string `yaml:"preset"`  // jwt-issuer: clerk
	Users   string `yaml:"users"`   // jwt-issuer: JSON file of users to sign a token for each
}

// Seed loads the same data into every stack after the app is healthy.
type Seed struct {
	SQL  string `yaml:"sql"`  // SQL file, run with psql in one session
	Into string `yaml:"into"` // postgres service to run it against
}

// Load configures the k6 run executed against baseline and candidate stacks.
// A task's own load settings override these field by field.
type Load struct {
	Script   string `yaml:"script"`   // k6 script, relative to the profile dir
	VUs      int    `yaml:"vus"`      // concurrent virtual users
	Duration string `yaml:"duration"` // k6 duration of one round, e.g. "10s"
	Rounds   int    `yaml:"rounds"`   // interleaved rounds per stack; perf compares medians
	// Env is passed to k6; values are templates over service outputs, plus
	// {{ .app.url }}.
	Env map[string]string `yaml:"env"`
}

// Stage is one check run on the worktree, in profile order.
type Stage struct {
	Name string `yaml:"name"` // build, vet, lint or test
	Run  string `yaml:"run"`  // shell command run in the worktree
	// Each, if set, is a shell command whose output lines are items; Run
	// then runs once per item with {item} replaced.
	Each    string `yaml:"each"`
	Gate    bool   `yaml:"gate"`    // a failure skips every later stage, the stack included
	Compare string `yaml:"compare"` // "baseline": fail only on findings the candidate added
	Results string `yaml:"results"` // reader for the command's output; "" means the exit code decides
	Timeout string `yaml:"timeout"` // per command, default 20m
}

// TimeoutDuration is the stage's per-command time limit.
func (s Stage) TimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(s.Timeout); err == nil {
		return d
	}
	return 20 * time.Minute
}

// Tests describes the project's test output.
type Tests struct {
	// InfraErrors are output fragments meaning the test infrastructure (a
	// test database, a container) failed, not the code under test. A failing
	// run containing one is an infra error rather than a verdict.
	InfraErrors []string `yaml:"infra_errors"`
}

// Agent is what an implementing agent may run in this project, on top of
// the core read/edit tools.
type Agent struct {
	Allow []string `yaml:"allow"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Read parses and validates dir/profile.yaml.
func Read(dir string) (*Profile, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(abs, "profile.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Profile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	p.Dir = abs
	if p.SHA256, err = hashDir(abs); err != nil {
		return nil, err
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("invalid profile %s: %w", path, err)
	}
	return &p, nil
}

func (p *Profile) validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	file := func(field, rel string) {
		if rel == "" {
			add("%s is required", field)
			return
		}
		if filepath.IsAbs(rel) || strings.HasPrefix(filepath.Clean(rel), "..") {
			add("%s must be a path inside the profile dir", field)
			return
		}
		if _, err := os.Stat(filepath.Join(p.Dir, rel)); err != nil {
			add("%s: %v", field, err)
		}
	}

	if !namePattern.MatchString(p.Name) {
		add("name must match %s", namePattern)
	}

	for name, s := range p.Services {
		if !namePattern.MatchString(name) || name == "app" || name == "seed" || name == "k6" {
			add("service name %q must match %s and not be app, seed or k6", name, namePattern)
		}
		switch s.Module {
		case ModulePostgres:
			if s.Version == "" {
				add("services.%s: postgres needs a pinned version", name)
			}
			if s.Preset != "" || s.Users != "" {
				add("services.%s: postgres takes only version", name)
			}
		case ModuleJWTIssuer:
			if s.Preset != "clerk" {
				add("services.%s: jwt-issuer preset must be clerk", name)
			}
			if s.Version != "" {
				add("services.%s: jwt-issuer takes no version", name)
			}
			if s.Users != "" {
				file("services."+name+".users", s.Users)
			}
		default:
			add("services.%s: unknown module %q (want postgres or jwt-issuer)", name, s.Module)
		}
	}

	if p.App != nil {
		file("app.dockerfile", p.App.Dockerfile)
		if p.App.Port <= 0 {
			add("app.port is required")
		}
		if !strings.HasPrefix(p.App.Health, "/") {
			add("app.health must be an HTTP path")
		}
		for k, v := range p.App.Env {
			if err := p.checkTemplate(v); err != nil {
				add("app.env.%s: %v", k, err)
			}
		}
	}
	if p.Seed != nil {
		if p.App == nil {
			add("seed needs an app")
		}
		file("seed.sql", p.Seed.SQL)
		if s, ok := p.Services[p.Seed.Into]; !ok || s.Module != ModulePostgres {
			add("seed.into must name a postgres service")
		}
	}
	if p.Load != nil {
		if p.App == nil {
			add("load needs an app")
		}
		file("load.script", p.Load.Script)
		if p.Load.VUs < 1 || p.Load.Rounds < 1 || p.Load.Duration == "" {
			add("load needs vus, duration and rounds")
		}
		for k, v := range p.Load.Env {
			if err := p.checkTemplate(v); err != nil {
				add("load.env.%s: %v", k, err)
			}
		}
	}
	p.validateStages(add)

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

func (p *Profile) validateStages(add func(string, ...any)) {
	seen := map[string]bool{}
	for i, s := range p.Stages {
		where := fmt.Sprintf("stages[%d] (%s)", i, s.Name)
		if seen[s.Name] {
			add("%s: stage names must be unique", where)
		}
		seen[s.Name] = true
		if strings.TrimSpace(s.Run) == "" {
			add("%s: run is required", where)
		}
		if s.Each != "" && !strings.Contains(s.Run, "{item}") {
			add("%s: run must use {item} when each is set", where)
		}
		if s.Timeout != "" {
			if _, err := time.ParseDuration(s.Timeout); err != nil {
				add("%s: timeout: %v", where, err)
			}
		}
		switch s.Compare {
		case "":
		case "baseline":
			if s.Results == "" {
				add("%s: compare: baseline needs a results reader that reports findings", where)
			}
		default:
			add("%s: compare must be baseline or empty", where)
		}
		switch s.Name {
		case StageBuild, StageVet:
			if s.Results != "" || s.Compare != "" {
				add("%s: build and vet are decided by their exit code", where)
			}
		case StageLint:
			if s.Results != "" && s.Results != ReaderStaticcheckJSON {
				add("%s: lint results must be %s or empty", where, ReaderStaticcheckJSON)
			}
		case StageTest:
			if s.Results != ReaderGoJSON {
				add("%s: test results must be %s", where, ReaderGoJSON)
			}
			if s.Compare != "" {
				add("%s: test cannot compare against the baseline", where)
			}
		default:
			add("%s: unknown stage (want build, vet, lint or test)", where)
		}
	}
	if !seen[StageTest] {
		add("a test stage is required: hidden tests are graded from its results")
	}
}

// checkTemplate renders v against placeholder outputs, so a typo in a
// service or output name fails when the profile is read, not mid-run.
func (p *Profile) checkTemplate(v string) error {
	outs := map[string]map[string]string{}
	for name, s := range p.Services {
		outs[name] = map[string]string{}
		for _, k := range OutputKeys(s) {
			outs[name][k] = "x"
		}
	}
	outs["app"] = map[string]string{"url": "x"}
	_, err := Render(v, outs)
	return err
}

// OutputKeys lists what a service provides to app.env and load.env templates.
func OutputKeys(s Service) []string {
	switch s.Module {
	case ModulePostgres:
		return []string{"url", "host"}
	case ModuleJWTIssuer:
		keys := []string{"host", "jwks_url", "ca_file", "tokens_file"}
		if s.Preset == "clerk" {
			keys = append(keys, "publishable_key")
		}
		return keys
	}
	return nil
}

// Render expands a template over service outputs. Unknown keys are errors.
func Render(v string, outputs map[string]map[string]string) (string, error) {
	t, err := template.New("value").Option("missingkey=error").Parse(v)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, outputs); err != nil {
		return "", err
	}
	return b.String(), nil
}

// AgentTools are the extra tools an implementing agent may use here.
func (p *Profile) AgentTools() []string {
	if p == nil {
		return nil
	}
	return slices.Clone(p.Agent.Allow)
}

// Stage returns the named stage, if the profile has it.
func (p *Profile) Stage(name string) (Stage, bool) {
	for _, s := range p.Stages {
		if s.Name == name {
			return s, true
		}
	}
	return Stage{}, false
}

// hashDir hashes every regular file under dir by relative path and content.
func hashDir(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		fmt.Fprintf(h, "%s:%d\n", filepath.ToSlash(rel), len(b))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

// Resolve returns the profile a task at commit is graded with: the project's
// .eval/ as of that commit, so an old task keeps the profile it was written
// against, or, for a commit made before the project had one, the .eval/ of
// the repo checkout itself. The commit's copy is extracted into dest, an
// empty dir the caller removes when done.
func Resolve(ctx context.Context, repo, commit, dest string) (*Profile, error) {
	err := exec.CommandContext(ctx, "git", "-C", repo, "cat-file", "-e", commit+":"+Dir+"/profile.yaml").Run()
	if err != nil {
		p, readErr := Read(filepath.Join(repo, Dir))
		if readErr != nil {
			return nil, fmt.Errorf("%s has no %s/profile.yaml at %s, and the checkout's can't be used: %w", repo, Dir, commit, readErr)
		}
		return p, nil
	}
	archive := exec.CommandContext(ctx, "git", "-C", repo, "archive", "--format=tar", commit, Dir)
	untar := exec.CommandContext(ctx, "tar", "-x", "-C", dest)
	if untar.Stdin, err = archive.StdoutPipe(); err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	archive.Stderr = &stderr
	if err := untar.Start(); err != nil {
		return nil, err
	}
	if err := archive.Run(); err != nil {
		_ = untar.Wait()
		return nil, fmt.Errorf("git archive %s:%s: %w: %s", commit, Dir, err, stderr.String())
	}
	if err := untar.Wait(); err != nil {
		return nil, fmt.Errorf("extract %s at %s: %w", Dir, commit, err)
	}
	return Read(filepath.Join(dest, Dir))
}
