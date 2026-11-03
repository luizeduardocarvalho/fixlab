package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/luizeduardocarvalho/fixlab/internal/implement"
)

// cmdSpec describes one external command. Output is always written to
// logs/<log>.log under the run's artifact dir so failures can be inspected
// after the worktrees are gone.
type cmdSpec struct {
	dir     string
	env     []string
	stdin   io.Reader
	timeout time.Duration
	log     string
	name    string
	args    []string
}

// exec runs spec, logging under the run's artifact dir.
func (r *run) exec(ctx context.Context, spec cmdSpec) ([]byte, error) {
	return execCmd(ctx, filepath.Join(r.artifacts, "logs"), spec)
}

// execCmd executes spec and returns stdout, appending the command and its
// output to logDir/<spec.log>.log. A non-zero exit is returned as an error
// alongside whatever stdout was produced, since several tools (go test,
// staticcheck) report results through stdout and signal them via exit code.
func execCmd(ctx context.Context, logDir string, spec cmdSpec) ([]byte, error) {
	if spec.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, spec.name, spec.args...)
	cmd.Dir = spec.dir
	cmd.Env = append(implement.SafeEnv(), spec.env...) // candidate code runs here: no production credentials
	cmd.Stdin = spec.stdin
	cmd.WaitDelay = 10 * time.Second

	logPath := filepath.Join(logDir, spec.log+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "$ (cd %s) %s %s\n", spec.dir, spec.name, strings.Join(spec.args, " "))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, logFile)
	cmd.Stderr = io.MultiWriter(&stderr, logFile)
	err = cmd.Run()
	if err != nil {
		tail := lastLines(stderr.String()+stdout.String(), 15)
		return stdout.Bytes(), fmt.Errorf("%s %s: %w\n%s", spec.name, strings.Join(spec.args, " "), err, tail)
	}
	return stdout.Bytes(), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
