// Package stack renders a project's local stack from its profile: one Compose
// file per stack with the profile's services (built-in modules), the app
// built from a worktree, and on-demand seed and k6 tools.
//
// No service publishes a host port unless asked, so any number of stacks run
// side by side; each Compose project gets its own network.
package stack

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/luizeduardocarvalho/fixlab/internal/profile"
)

// Pinned images of the tools the harness runs.
const (
	k6Image    = "grafana/k6:0.54.0"
	nginxImage = "nginx:1.27.2-alpine"
)

// Mount points inside every container that needs them.
const (
	assetsMount  = "/eval"    // the run's module assets (keys, certs, tokens)
	profileMount = "/profile" // the profile dir (load script)
	outMount     = "/out"     // where the load script writes summary.json
)

// Tool services, run on demand with `compose run`.
const (
	SeedService = "seed"
	LoadService = "k6"
	AppService  = "app"
)

// Outputs maps each service to what it provides (profile.OutputKeys), plus
// "app" → {"url"}: the values app.env and load.env templates see.
type Outputs map[string]map[string]string

// Spec is one stack to render.
type Spec struct {
	Profile  *profile.Profile
	Worktree string // the app's build context
	Assets   string // dir Prepare wrote module assets to
	Out      string // dir the load script writes its summary to
	// PublishApp publishes the app's port on 127.0.0.1 at a free host port,
	// for a stack a person or agent uses directly. Grading stacks publish
	// nothing.
	PublishApp bool
}

// Prepare writes every module's assets under assets/<service>/ and returns
// the outputs as seen from inside the stack. Stacks that are compared must
// share one Prepare, so they accept the same tokens.
func Prepare(p *profile.Profile, assets string) (Outputs, error) {
	outs := Outputs{}
	for _, name := range serviceNames(p) {
		s := p.Services[name]
		var err error
		switch s.Module {
		case profile.ModulePostgres:
			outs[name] = postgresOutputs(name)
		case profile.ModuleJWTIssuer:
			outs[name], err = prepareIssuer(p, name, s, filepath.Join(assets, name))
		default:
			err = fmt.Errorf("unknown module %q", s.Module)
		}
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", name, err)
		}
	}
	if p.App != nil {
		outs[AppService] = map[string]string{"url": fmt.Sprintf("http://%s:%d", AppService, p.App.Port)}
	}
	return outs, nil
}

// Compose renders the Compose file for one stack.
func Compose(spec Spec, outs Outputs) ([]byte, error) {
	p := spec.Profile
	services := map[string]any{}
	healthy := map[string]any{}
	for _, name := range serviceNames(p) {
		s := p.Services[name]
		switch s.Module {
		case profile.ModulePostgres:
			services[name] = postgresService(s)
		case profile.ModuleJWTIssuer:
			services[name] = issuerService(name, spec.Assets)
		}
		healthy[name] = map[string]any{"condition": "service_healthy"}
	}

	if p.App != nil {
		env, err := renderEnv(p.App.Env, outs)
		if err != nil {
			return nil, fmt.Errorf("app.env: %w", err)
		}
		app := map[string]any{
			"build": map[string]any{
				"context":             spec.Worktree,
				"dockerfile":          filepath.Join(p.Dir, p.App.Dockerfile),
				"additional_contexts": map[string]any{"profile": p.Dir},
			},
			"environment": env,
			"volumes":     []string{spec.Assets + ":" + assetsMount + ":ro"},
			"healthcheck": healthcheck(fmt.Sprintf("wget -qO- http://127.0.0.1:%d%s", p.App.Port, p.App.Health), 90),
		}
		if len(healthy) > 0 {
			app["depends_on"] = healthy
		}
		if spec.PublishApp {
			app["ports"] = []string{"127.0.0.1::" + strconv.Itoa(p.App.Port)}
		}
		services[AppService] = app
		appHealthy := map[string]any{AppService: map[string]any{"condition": "service_healthy"}}

		if p.Seed != nil {
			services[SeedService] = map[string]any{
				"image":       postgresImage(p.Services[p.Seed.Into]),
				"profiles":    []string{"tools"},
				"depends_on":  appHealthy,
				"environment": map[string]string{"PGPASSWORD": pgPassword},
				"entrypoint":  []string{"psql", "-h", p.Seed.Into, "-U", pgUser, "-d", pgDatabase, "-v", "ON_ERROR_STOP=1", "-q", "-f", "-"},
			}
		}
		if p.Load != nil {
			env, err := renderEnv(p.Load.Env, outs)
			if err != nil {
				return nil, fmt.Errorf("load.env: %w", err)
			}
			services[LoadService] = map[string]any{
				"image":       k6Image,
				"profiles":    []string{"tools"},
				"depends_on":  appHealthy,
				"environment": env,
				"volumes": []string{
					p.Dir + ":" + profileMount + ":ro",
					spec.Assets + ":" + assetsMount + ":ro",
					spec.Out + ":" + outMount,
				},
			}
		}
	}
	return yaml.Marshal(map[string]any{"services": services})
}

// LoadScript is the path of a profile-relative k6 script inside the k6 container.
func LoadScript(rel string) string {
	return profileMount + "/" + filepath.ToSlash(filepath.Clean(rel))
}

// Infrastructure services, in a stable order: the services a stack waits
// for before the app starts.
func InfraServices(p *profile.Profile) []string {
	names := serviceNames(p)
	if p.App != nil {
		names = append(names, AppService)
	}
	return names
}

func serviceNames(p *profile.Profile) []string {
	names := make([]string, 0, len(p.Services))
	for n := range p.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// renderEnv expands templates and escapes "$", which Compose would otherwise
// interpolate.
func renderEnv(env map[string]string, outs Outputs) (map[string]string, error) {
	rendered := make(map[string]string, len(env))
	for k, v := range env {
		val, err := profile.Render(v, outs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		rendered[k] = strings.ReplaceAll(val, "$", "$$")
	}
	return rendered, nil
}

func healthcheck(cmd string, retries int) map[string]any {
	return map[string]any{
		"test":     []string{"CMD-SHELL", cmd},
		"interval": "2s",
		"timeout":  "3s",
		"retries":  retries,
	}
}
