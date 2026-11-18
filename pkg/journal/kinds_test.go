package journal

import (
	"net/http"
	"os"
	"slices"
	"testing"
)

// through runs one Pulumi command against the journal, as SST does for every
// command but diff.
func through(t *testing.T, dir, cli string, env []string, args ...string) {
	t.Helper()
	server, err := Start(Options{StatePath: statePath(dir), Org: "organization", Project: "bench", Stack: "dev", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	run(t, dir, cli, slices.Concat(env, server.Env()), slices.Concat(args, []string{"--non-interactive", "--stack", "organization/bench/dev"})...)
	server.Close()
}

// sst refresh and sst remove go through the journal too: a refresh and a
// destroy are created at their own kind, then addressed as updates.
func TestRefreshAndDestroyThroughTheJournal(t *testing.T) {
	cli, env := pulumiEnv(t)
	dir := t.TempDir()
	diy := slices.Concat(env, []string{"PULUMI_BACKEND_URL=file://" + dir})
	project(t, dir, 5, func(int) int { return 1 })
	run(t, dir, cli, diy, "stack", "init", "organization/bench/dev")
	up(t, dir, cli, env)
	if err := os.Remove(dir + "/Pulumi.dev.yaml"); err != nil {
		t.Fatal(err)
	}

	// The arguments sst refresh and sst remove pass.
	through(t, dir, cli, env, "refresh", "--yes", "--run-program")
	checkResources(t, dir, 5)
	run(t, dir, cli, diy, "preview", "--expect-no-changes", "--non-interactive", "--stack", "organization/bench/dev")

	through(t, dir, cli, env, "destroy", "--yes", "-f")
	checkResources(t, dir, 0)
}

// An update is reachable under "update", as the CLI addresses it, and under
// the kind it was created with, and under no other kind.
func TestUpdatePaths(t *testing.T) {
	s, _ := startOn(t)
	code, body := call(t, s, "POST", stackPath+"/refresh", user(s), program, false)
	if code != http.StatusOK {
		t.Fatalf("creating a refresh: %d %s", code, body)
	}
	var created struct{ UpdateID string }
	decodeInto(t, body, &created)
	start := map[string]any{"journalVersion": 2}
	steps := []struct {
		kind string
		want int
	}{
		{"destroy", http.StatusNotFound},
		{"update", http.StatusOK},
		// Found, and already started.
		{"refresh", http.StatusConflict},
	}
	for _, step := range steps {
		code, body := call(t, s, "POST", stackPath+"/"+step.kind+"/"+created.UpdateID, user(s), start, false)
		if code != step.want {
			t.Errorf("starting a refresh at /%s/: %d %s, want %d", step.kind, code, body, step.want)
		}
	}
}
