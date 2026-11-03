package strategy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lean.yaml")
	if err := os.WriteFile(path, []byte("name: lean\nprompt: prompts/x.md\nallow: [Task]\ntimeout: 45m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Read(path)
	if err != nil || s.Name != "lean" || len(s.Allow) != 1 || len(s.SHA256) != 64 {
		t.Fatalf("got %+v, %v", s, err)
	}
	if err := os.WriteFile(path, []byte("name: other\nmodel: m\ntimeout: soon\nunknown: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("want unknown-field error, got %v", err)
	}
	if err := os.WriteFile(path, []byte("name: other\ntimeout: soon\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Read(path)
	for _, want := range []string{`name must be "lean"`, "prompt is required", "timeout"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q in %v", want, err)
		}
	}
}

func TestResolve(t *testing.T) {
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{
		"":               "",
		"none":           "none",
		"/abs/x":         "/abs/x",
		"~/plugins/p":    filepath.Join(home, "plugins/p"),
		"claude-md/t.md": "/eval/claude-md/t.md",
	} {
		if got := Resolve(in, "/eval"); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", in, got, want)
		}
	}
}
