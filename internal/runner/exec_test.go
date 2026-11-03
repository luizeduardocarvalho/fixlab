package runner

import (
	"context"
	"strings"
	"testing"
)

// The harness builds, tests and serves agent-written code through exec; a
// production DATABASE_URL in the harness's own shell must not reach it.
func TestExec_KeepsProductionCredentialsFromCandidateCode(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@prod.example/db")
	t.Setenv("CLERK_SECRET_KEY", "sk_live_x")
	r := &run{artifacts: t.TempDir()}

	out, err := r.exec(context.Background(), cmdSpec{
		dir: t.TempDir(), log: "env", name: "env",
		env: []string{"STAGE_VAR=kept"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, leaked := range []string{"prod.example", "sk_live_x"} {
		if strings.Contains(got, leaked) {
			t.Errorf("candidate env contains %q", leaked)
		}
	}
	if !strings.Contains(got, "STAGE_VAR=kept") {
		t.Error("stage-specific env was dropped")
	}
}
