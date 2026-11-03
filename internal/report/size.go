package report

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/luizeduardocarvalho/fixlab/internal/task"
)

// Size describes how big a task is, measured on its reference fix
// (base_commit..reference_commit). Test files are counted apart: they are
// what proves the change, not the change itself.
type Size struct {
	ProdLines   int    `json:"prod_lines"` // added + removed outside test files
	ProdFiles   int    `json:"prod_files"`
	ProdHunks   int    `json:"prod_hunks"`
	Functions   int    `json:"functions"` // distinct functions whose bodies the fix touches (from hunk headers)
	TestLines   int    `json:"test_lines"`
	IssueWords  int    `json:"issue_words"`
	HiddenTests int    `json:"hidden_tests"`
	Class       string `json:"class"`
}

// Classes are ordered smallest first. A task's class is the largest one any
// of its measures reaches, so a 20-line change spread over 6 files is not
// "XS".
var Classes = []struct {
	Name               string
	MaxLines, MaxFiles int
	MaxFunctions       int
	Description        string
}{
	{"XS", 25, 1, 1, "≤ 25 lines, 1 file, 1 function"},
	{"S", 100, 2, 3, "≤ 100 lines, ≤ 2 files, ≤ 3 functions"},
	{"M", 300, 5, 8, "≤ 300 lines, ≤ 5 files, ≤ 8 functions"},
	{"L", 800, 12, 20, "≤ 800 lines, ≤ 12 files, ≤ 20 functions"},
	{"XL", -1, -1, -1, "anything larger"},
}

func classOf(s Size) string {
	for _, c := range Classes {
		if c.MaxLines < 0 || (s.ProdLines <= c.MaxLines && s.ProdFiles <= c.MaxFiles && s.Functions <= c.MaxFunctions) {
			return c.Name
		}
	}
	return Classes[len(Classes)-1].Name
}

// isTestFile reports whether path holds tests in the given repo's conventions.
func isTestFile(repo, path string) bool {
	if repo == task.RepoWeb {
		return strings.Contains(path, ".test.") || strings.Contains(path, ".spec.") || strings.Contains(path, "__tests__/")
	}
	return strings.HasSuffix(path, "_test.go") || strings.Contains(path, "/testdata/")
}

// hunkFunc pulls the function name out of a hunk header's context, e.g.
// "@@ -1,2 +1,3 @@ func (h *ItemHandler) UpdateItem(w http.ResponseWriter" → UpdateItem.
var hunkFunc = regexp.MustCompile(`^@@ [^@]+ @@ .*?(?:func\s+(?:\([^)]*\)\s*)?|function\s+|const\s+)([A-Za-z_$][\w$]*)`)

// Measure sizes a task from its reference fix in repoDir.
func Measure(t task.Task, repoDir string) (Size, error) {
	s := Size{IssueWords: len(strings.Fields(t.Description))}
	if names, err := t.HiddenGoTests(); err == nil {
		s.HiddenTests = len(names)
	}
	if t.ReferenceCommit == "" {
		return s, fmt.Errorf("task %s has no reference_commit", t.ID)
	}
	numstat, err := git(repoDir, "diff", "--numstat", t.BaseCommit, t.ReferenceCommit)
	if err != nil {
		return s, err
	}
	var prod []string
	sc := bufio.NewScanner(bytes.NewReader(numstat))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		added, _ := strconv.Atoi(f[0])
		removed, _ := strconv.Atoi(f[1])
		if isTestFile(t.Repo, f[2]) {
			s.TestLines += added + removed
			continue
		}
		s.ProdLines += added + removed
		prod = append(prod, f[2])
	}
	s.ProdFiles = len(prod)
	if len(prod) > 0 {
		hunks, err := git(repoDir, append([]string{"diff", "-U0", t.BaseCommit, t.ReferenceCommit, "--"}, prod...)...)
		if err != nil {
			return s, err
		}
		funcs := map[string]bool{}
		sc := bufio.NewScanner(bytes.NewReader(hunks))
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		file := ""
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "+++ ") {
				file = strings.TrimPrefix(line, "+++ ")
				continue
			}
			if !strings.HasPrefix(line, "@@ ") {
				continue
			}
			s.ProdHunks++
			if m := hunkFunc.FindStringSubmatch(line); m != nil {
				funcs[file+"#"+m[1]] = true
			}
		}
		s.Functions = len(funcs)
	}
	s.Class = classOf(s)
	return s, nil
}

func git(dir string, args ...string) ([]byte, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr)
	}
	return out, nil
}
