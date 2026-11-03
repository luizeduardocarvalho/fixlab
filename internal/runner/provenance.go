package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/luizeduardocarvalho/fixlab/internal/implement"
)

func (r *run) recordImplementer(info implement.Info) {
	s := r.res
	s.ImplementerKind = info.Kind
	if info.Kind != "agent" {
		return
	}
	s.Model, s.Effort, s.AgentCLIVersion = info.Model, info.Effort, info.CLIVersion
	s.PromptVariant, s.PromptSHA256 = info.PromptVariant, info.PromptSHA256
	s.ClaudeMDVariant, s.ClaudeMDSHA256 = info.ClaudeMDVariant, info.ClaudeMDSHA256
	s.AgentPlugins, s.AgentPluginsSHA256, s.AgentSkillsUsed = info.Plugins, info.PluginsSHA256, info.SkillsUsed
	s.AgentSetup = info.Setup
	s.AgentOutcome = info.Outcome
	if info.Outcome == "" {
		return // the session never produced a result; usage is unknown
	}
	s.AgentCostUSD, s.AgentDurationSec = ptr(info.CostUSD), ptr(round(info.DurationSec))
	s.AgentInputTokens, s.AgentOutputTokens = intPtr(info.InputTokens), intPtr(info.OutputTokens)
	s.AgentCacheReadTok, s.AgentCacheWriteTok = intPtr(info.CacheReadTokens), intPtr(info.CacheWriteTokens)
	s.AgentTurns, s.AgentDeniedPathRefs = intPtr(info.Turns), intPtr(info.DeniedPathRefs)
}

// measureDiff records the size of the candidate change, before hidden tests
// or the entrypoint are added, and how many changed files the reference fix
// didn't touch.
func (r *run) measureDiff(ctx context.Context, repo string) error {
	if _, err := r.exec(ctx, cmdSpec{log: "git-diff", name: "git", args: []string{"-C", r.candWT, "add", "-A"}}); err != nil {
		return err
	}
	out, err := r.exec(ctx, cmdSpec{log: "git-diff", name: "git", args: []string{"-C", r.candWT, "diff", "--cached", "--numstat"}})
	if err != nil {
		return err
	}
	var files []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		added, _ := strconv.Atoi(f[0]) // "-" for binary files counts as 0
		removed, _ := strconv.Atoi(f[1])
		r.res.DiffLinesAdded += added
		r.res.DiffLinesRemoved += removed
		files = append(files, f[2])
	}
	r.res.DiffFiles = len(files)

	if r.task.ReferenceCommit == "" {
		return nil
	}
	ref, err := r.exec(ctx, cmdSpec{log: "git-diff", name: "git", args: []string{"-C", repo, "diff", "--name-only", r.task.BaseCommit, r.task.ReferenceCommit}})
	if err != nil {
		return err
	}
	inRef := map[string]bool{}
	for _, f := range strings.Fields(string(ref)) {
		inRef[f] = true
	}
	outside := 0
	for _, f := range files {
		if !inRef[f] {
			outside++
		}
	}
	r.res.DiffFilesOutsideReference = intPtr(outside)
	return nil
}

// HarnessVersion is the commit fixlab was built from, suffixed "+dirty" when
// the build had uncommitted changes, or its module version when it was
// installed without VCS information (go install …@version).
func HarnessVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}
		if dirty {
			rev += "+dirty"
		}
		return rev
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return "unknown"
}

// manifest holds per-attempt context that doesn't belong in the flat result.
type manifest struct {
	RunID          string             `json:"run_id"`
	RecordedAt     string             `json:"recorded_at"`
	Thresholds     map[string]float64 `json:"thresholds"`
	Load           any                `json:"load"`
	AllowedTools   []string           `json:"allowed_tools,omitempty"`
	DeniedPaths    int                `json:"denied_paths,omitempty"`
	Host           map[string]string  `json:"host"`
	HarnessVersion string             `json:"harness_version"`
}

// saveHistory copies the run's result and artifacts into
// <history>/attempts/<task_id>/<run_id>/ and, if the history dir is a git
// repo, commits them. It never pushes.
func (r *run) saveHistory(resultPath string) error {
	if r.cfg.HistoryDir == "" {
		return nil
	}
	dst := filepath.Join(r.cfg.HistoryDir, "attempts", r.task.ID, r.id)
	if err := copyTree(r.artifacts, dst); err != nil {
		return err
	}
	if err := copyFile(resultPath, filepath.Join(dst, "result.json")); err != nil {
		return err
	}
	m := manifest{
		RunID:      r.id,
		RecordedAt: time.Now().UTC().Format(time.RFC3339),
		Thresholds: map[string]float64{
			"p95_tolerance":        r.cfg.P95Tolerance,
			"p95_slack_ms":         r.cfg.P95SlackMs,
			"error_rate_tolerance": r.cfg.ErrorRateTolerance,
		},
		Load:           r.effectiveLoad(),
		AllowedTools:   r.cfg.AgentAllowedTools,
		DeniedPaths:    r.cfg.AgentDeniedPaths,
		Host:           hostInfo(r.cfg.GoBin),
		HarnessVersion: r.res.HarnessVersion,
	}
	if err := writeJSON(filepath.Join(dst, "manifest.json"), m); err != nil {
		return err
	}
	if !r.cfg.HistoryCommit {
		return nil
	}
	if _, err := os.Stat(filepath.Join(r.cfg.HistoryDir, ".git")); err != nil {
		return nil
	}
	rel, _ := filepath.Rel(r.cfg.HistoryDir, dst)
	msg := fmt.Sprintf("%s %s: %s", r.task.ID, r.res.Implementer, verdict(r.res.Pass, r.res.HasInfraError(), r.res.FailureStage))
	if r.res.RegradedFrom != "" {
		msg += " (regrade of " + r.res.RegradedFrom + ")"
	}
	for _, args := range [][]string{{"add", "--", rel}, {"commit", "-q", "--no-verify", "-m", msg, "--", rel}} {
		if out, err := exec.Command("git", append([]string{"-C", r.cfg.HistoryDir}, args...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("history git %v: %w: %s", args, err, out)
		}
	}
	return nil
}

// effectiveLoad is the load configuration the run used: the profile's with
// the task's overrides, or the task's own when there is no profile load.
func (r *run) effectiveLoad() any {
	if r.cfg.Profile == nil || r.cfg.Profile.Load == nil {
		return r.task.Load
	}
	return r.load()
}

func verdict(pass, infra bool, stage string) string {
	switch {
	case pass:
		return "pass"
	case infra:
		return "error at " + stage
	default:
		return "fail at " + stage
	}
}

// hostInfo describes the machine coarsely; perf numbers are only comparable
// between attempts recorded on the same kind of host.
func hostInfo(goBin string) map[string]string {
	info := map[string]string{"os": runtime.GOOS, "arch": runtime.GOARCH, "cpus": strconv.Itoa(runtime.NumCPU())}
	if out, err := exec.Command(goBin, "env", "GOVERSION").Output(); err == nil {
		info["go"] = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output(); err == nil {
		info["docker"] = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("docker", "info", "--format", "{{.MemTotal}}").Output(); err == nil {
		info["docker_mem_bytes"] = strings.TrimSpace(string(out))
	}
	return info
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func intPtr(v int) *int { return &v }

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
