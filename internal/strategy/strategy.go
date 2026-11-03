// Package strategy loads named agent configurations: everything that decides
// how an agent works on a task, in one versioned file.
package strategy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Strategy is <workspace>/strategies/<name>.yaml. Paths are relative to the
// workspace; a plugin path may start with ~/.
type Strategy struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Model       string   `yaml:"model"`
	Effort      string   `yaml:"effort"`
	Prompt      string   `yaml:"prompt"`
	ClaudeMD    string   `yaml:"claude_md"` // "" = the repo's, "none", or a file
	Plugins     []string `yaml:"plugins"`
	Allow       []string `yaml:"allow"`  // tools added to the default set
	Setup       string   `yaml:"setup"`  // shell command run in the snapshot first
	Ignore      []string `yaml:"ignore"` // kept out of the diff
	BudgetUSD   float64  `yaml:"budget_usd"`
	Timeout     string   `yaml:"timeout"`

	SHA256 string `yaml:"-"` // of the file, recorded with every attempt
}

// Read parses and validates a strategy file.
func Read(path string) (Strategy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Strategy{}, err
	}
	var s Strategy
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return Strategy{}, fmt.Errorf("parse %s: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	s.SHA256 = hex.EncodeToString(sum[:])

	var problems []string
	if want := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)); s.Name != want {
		problems = append(problems, fmt.Sprintf("name must be %q (the file name)", want))
	}
	if s.Prompt == "" {
		problems = append(problems, "prompt is required")
	}
	if s.Timeout != "" {
		if _, err := time.ParseDuration(s.Timeout); err != nil {
			problems = append(problems, "timeout: "+err.Error())
		}
	}
	if s.BudgetUSD < 0 {
		problems = append(problems, "budget_usd must be >= 0")
	}
	if len(problems) > 0 {
		return Strategy{}, fmt.Errorf("invalid strategy %s: %s", path, strings.Join(problems, "; "))
	}
	return s, nil
}

// Resolve turns a path from the file into an absolute one: ~/ is the home
// dir, anything else relative is under workspace. "" and "none" pass through.
func Resolve(path, workspace string) string {
	switch {
	case path == "" || path == "none" || filepath.IsAbs(path):
		return path
	case strings.HasPrefix(path, "~/"):
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	default:
		return filepath.Join(workspace, path)
	}
}
