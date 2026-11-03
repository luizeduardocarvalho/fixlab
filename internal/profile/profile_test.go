package profile

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRead_RepoProfile(t *testing.T) {
	p, err := Read("../../testdata/example/.eval")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "example-api" || p.App == nil || p.Seed == nil || p.Load == nil || len(p.SHA256) != 64 {
		t.Fatalf("unexpected profile: %+v", p)
	}
	if s, ok := p.Stage(StageTest); !ok || s.Each == "" || s.Results != ReaderGoJSON {
		t.Fatalf("test stage: %+v", s)
	}
}

// minimal is the smallest valid profile: a test stage and nothing else.
const minimal = "name: demo\nstages:\n  - { name: test, run: go test -json ./..., results: go-json }\n"

func writeProfile(t *testing.T, yaml string, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRead_Minimal(t *testing.T) {
	p, err := Read(writeProfile(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if p.App != nil || p.Load != nil || len(p.AgentTools()) != 0 {
		t.Fatalf("unexpected defaults: %+v", p)
	}
}

func TestRead_Rejects(t *testing.T) {
	app := "app: { dockerfile: Dockerfile, port: 8080, health: /health, env: %s }\n"
	cases := map[string]struct {
		yaml  string
		files []string
		want  string
	}{
		"unknown field":  {minimal + "extra: 1\n", nil, "field extra not found"},
		"no test stage":  {"name: demo\nstages:\n  - { name: build, run: make }\n", nil, "a test stage is required"},
		"unknown stage":  {minimal + "  - { name: deploy, run: make }\n", nil, "unknown stage"},
		"duplicate":      {minimal + "  - { name: test, run: x, results: go-json }\n", nil, "must be unique"},
		"each w/o item":  {"name: demo\nstages:\n  - { name: test, each: ls, run: go test, results: go-json }\n", nil, "must use {item}"},
		"compare w/o rd": {minimal + "  - { name: lint, run: x, compare: baseline }\n", nil, "needs a results reader"},
		"bad module":     {minimal + "services:\n  cache: { module: redis }\n", nil, "unknown module"},
		"unpinned pg":    {minimal + "services:\n  db: { module: postgres }\n", nil, "pinned version"},
		"reserved name":  {minimal + "services:\n  seed: { module: postgres, version: '16' }\n", nil, "not be app, seed or k6"},
		"bad template": {minimal + "services:\n  db: { module: postgres, version: '16' }\n" + strings.Replace(app, "%s", `{ DB: "{{ .db.password }}" }`, 1),
			[]string{"Dockerfile"}, "app.env.DB"},
		"missing file":  {minimal + strings.Replace(app, "%s", "{}", 1), nil, "app.dockerfile"},
		"escaping path": {minimal + strings.Replace(app, "%s", "{}", 1) + "load: { script: ../load.js, vus: 1, duration: 1s, rounds: 1 }\n", []string{"Dockerfile"}, "load.script must be a path inside"},
		"seed into issuer": {minimal + "services:\n  auth: { module: jwt-issuer, preset: clerk }\n" + strings.Replace(app, "%s", "{}", 1) + "seed: { sql: s.sql, into: auth }\n",
			[]string{"Dockerfile", "s.sql"}, "seed.into must name a postgres service"},
	}
	for name, c := range cases {
		_, err := Read(writeProfile(t, c.yaml, c.files...))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", name, c.want, err)
		}
	}
}

func TestRead_HashChangesWithAnyFile(t *testing.T) {
	dir := writeProfile(t, minimal, "seed.sql")
	before, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "seed.sql"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before.SHA256 == after.SHA256 {
		t.Fatal("changing a profile file must change the profile hash")
	}
}

func TestResolve_CommitProfileElseCheckout(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "before profiles")
	before := git("rev-parse", "HEAD")

	if err := os.MkdirAll(filepath.Join(repo, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string) {
		yaml := strings.Replace(minimal, "demo", name, 1)
		if err := os.WriteFile(filepath.Join(repo, Dir, "profile.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("committed")
	git("add", ".")
	git("commit", "-q", "-m", "add profile")
	after := git("rev-parse", "HEAD")
	write("checkout") // uncommitted: only the checkout has it

	for commit, want := range map[string]string{before: "checkout", after: "committed"} {
		p, err := Resolve(context.Background(), repo, commit, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if p.Name != want {
			t.Errorf("at %s: got profile %q, want %q", commit[:7], p.Name, want)
		}
	}
}
