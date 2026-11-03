package stack

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/luizeduardocarvalho/fixlab/internal/profile"
)

func repoProfile(t *testing.T) *profile.Profile {
	t.Helper()
	p, err := profile.Read("../../testdata/example/.eval")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type composeFile struct {
	Services map[string]struct {
		Image       string            `yaml:"image"`
		Environment map[string]string `yaml:"environment"`
		Ports       []string          `yaml:"ports"`
		Volumes     []string          `yaml:"volumes"`
		Entrypoint  []string          `yaml:"entrypoint"`
		Profiles    []string          `yaml:"profiles"`
		DependsOn   map[string]any    `yaml:"depends_on"`
		Build       struct {
			Context            string            `yaml:"context"`
			Dockerfile         string            `yaml:"dockerfile"`
			AdditionalContexts map[string]string `yaml:"additional_contexts"`
		} `yaml:"build"`
	} `yaml:"services"`
}

func render(t *testing.T, p *profile.Profile, publish bool) (composeFile, Outputs) {
	t.Helper()
	assets := t.TempDir()
	outs, err := Prepare(p, assets)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Compose(Spec{Profile: p, Worktree: "/wt", Assets: assets, Out: "/out-dir", PublishApp: publish}, outs)
	if err != nil {
		t.Fatal(err)
	}
	var c composeFile
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	return c, outs
}

func TestCompose_RepoProfile(t *testing.T) {
	p := repoProfile(t)
	c, outs := render(t, p, false)
	for _, name := range []string{"postgres", "clerk", "app", "seed", "k6"} {
		if _, ok := c.Services[name]; !ok {
			t.Fatalf("missing service %s", name)
		}
	}
	app := c.Services["app"]
	if app.Build.Context != "/wt" || app.Build.AdditionalContexts["profile"] != p.Dir || app.Build.Dockerfile != filepath.Join(p.Dir, "api.Dockerfile") {
		t.Fatalf("app build: %+v", app.Build)
	}
	if got := app.Environment["DATABASE_URL"]; got != "postgres://eval:eval@postgres:5432/eval?sslmode=disable" {
		t.Fatalf("DATABASE_URL: %q", got)
	}
	key := app.Environment["CLERK_PUBLISHABLE_KEY"]
	host, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, "pk_test_"))
	if err != nil || string(host) != "clerk.eval.local$" {
		t.Fatalf("publishable key %q decodes to %q (%v)", key, host, err)
	}
	if app.Environment["SSL_CERT_FILE"] != "/eval/clerk/tls.crt" || outs["clerk"]["ca_file"] != "/eval/clerk/tls.crt" {
		t.Fatalf("ca file: %q", app.Environment["SSL_CERT_FILE"])
	}
	if len(app.Ports) != 0 {
		t.Fatalf("a grading stack must not publish ports: %v", app.Ports)
	}
	if len(app.DependsOn) != 2 {
		t.Fatalf("app must wait for both services: %v", app.DependsOn)
	}
	k6 := c.Services["k6"]
	if k6.Environment["API_URL"] != "http://app:8080" || k6.Environment["TOKENS_FILE"] != "/eval/clerk/tokens.json" {
		t.Fatalf("k6 env: %v", k6.Environment)
	}
	if LoadScript(p.Load.Script) != "/profile/k6/load.js" {
		t.Fatalf("load script path: %s", LoadScript(p.Load.Script))
	}
	if seed := c.Services["seed"]; seed.Image != "postgres:16.4-alpine3.20" || !strings.Contains(strings.Join(seed.Entrypoint, " "), "-h postgres") {
		t.Fatalf("seed: %+v", seed)
	}
}

func TestCompose_PublishesAppOnlyWhenAsked(t *testing.T) {
	c, _ := render(t, repoProfile(t), true)
	if got := c.Services["app"].Ports; len(got) != 1 || got[0] != "127.0.0.1::8080" {
		t.Fatalf("ports: %v", got)
	}
}

func TestCompose_EscapesDollarsInEnv(t *testing.T) {
	p := *repoProfile(t)
	app := *p.App
	app.Env = map[string]string{"SECRET": "a$b"}
	p.App = &app
	c, _ := render(t, &p, false)
	// Compose turns $$ back into $; a bare $b would be interpolated away.
	if got := c.Services["app"].Environment["SECRET"]; got != "a$$b" {
		t.Fatalf("got %q", got)
	}
}

func TestIssuer_TokensVerifyAgainstJWKS(t *testing.T) {
	p := repoProfile(t)
	assets := t.TempDir()
	if _, err := Prepare(p, assets); err != nil {
		t.Fatal(err)
	}
	var jwks struct {
		Keys []struct{ N, E string }
	}
	readJSON(t, filepath.Join(assets, "clerk", "jwks.json"), &jwks)
	n, _ := base64.RawURLEncoding.DecodeString(jwks.Keys[0].N)
	e, _ := base64.RawURLEncoding.DecodeString(jwks.Keys[0].E)
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}

	var tokens []map[string]any
	readJSON(t, filepath.Join(assets, "clerk", "tokens.json"), &tokens)
	var users []User
	readJSON(t, filepath.Join(p.Dir, "users.json"), &users)
	if len(tokens) != len(users) || len(tokens) == 0 {
		t.Fatalf("%d tokens for %d users", len(tokens), len(users))
	}
	for i, tok := range tokens {
		parts := strings.Split(tok["token"].(string), ".")
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
			t.Fatalf("token %d does not verify: %v", i, err)
		}
		payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims map[string]any
		if err := json.Unmarshal(payload, &claims); err != nil {
			t.Fatal(err)
		}
		if claims["sub"] != users[i].Claims["sub"] || claims["exp"] == nil {
			t.Fatalf("token %d claims: %v", i, claims)
		}
		if tok["lab_id"] != users[i].Data["lab_id"] {
			t.Fatalf("token %d lost its data: %v", i, tok)
		}
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}
