package journal

import (
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
)

func secret(ciphertext string) map[string]any {
	return map[string]any{resource.SigKey: resource.SecretSig, "ciphertext": ciphertext}
}

func resourceWith(outputs map[string]any, deps ...resource.URN) *apitype.ResourceV3 {
	return &apitype.ResourceV3{
		URN:          "urn:pulumi:dev::bench::random:index/randomString:RandomString::r0",
		Type:         "random:index/randomString:RandomString",
		Custom:       true,
		ID:           "abc",
		Outputs:      outputs,
		Dependencies: deps,
	}
}

func TestSameResource(t *testing.T) {
	base := resourceWith(map[string]any{"result": "abc", "keepers": secret("v1:aaa:111")}, "urn:a", "urn:b")

	cases := []struct {
		name string
		next *apitype.ResourceV3
		want bool
	}{
		{"the same", resourceWith(map[string]any{"result": "abc", "keepers": secret("v1:aaa:111")}, "urn:a", "urn:b"), true},
		{"a secret encrypted again", resourceWith(map[string]any{"result": "abc", "keepers": secret("v1:bbb:222")}, "urn:a", "urn:b"), true},
		{"dependencies in another order", resourceWith(map[string]any{"result": "abc", "keepers": secret("v1:aaa:111")}, "urn:b", "urn:a"), true},
		{"another output", resourceWith(map[string]any{"result": "xyz", "keepers": secret("v1:aaa:111")}, "urn:a", "urn:b"), false},
		{"a secret that became plain", resourceWith(map[string]any{"result": "abc", "keepers": "plain"}, "urn:a", "urn:b"), false},
		{"another dependency", resourceWith(map[string]any{"result": "abc", "keepers": secret("v1:aaa:111")}, "urn:a", "urn:c"), false},
	}
	for _, c := range cases {
		if got := sameResource(base, c.next); got != c.want {
			t.Errorf("%s: sameResource = %v, want %v", c.name, got, c.want)
		}
	}

	moved := *base
	moved.SourcePosition = "project:///index.ts#12,3"
	if !sameResource(base, &moved) {
		t.Error("a new source position counts as a change, mustWrite ignores it")
	}
	protected := *base
	protected.Protect = true
	if sameResource(base, &protected) {
		t.Error("protecting a resource is a change")
	}
}

// saving runs an update through the journal while SST would push: a save
// every few milliseconds, as the events come in. It returns how many of
// those saves wrote the state while the update ran.
func saving(t *testing.T, dir, cli string, env []string) (*Server, int) {
	t.Helper()
	server, err := Start(Options{StatePath: statePath(dir), Org: "organization", Project: "bench", Stack: "dev", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	written := 0
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				server.mu.Lock()
				before, finished := server.stats.saves, server.latest != nil && server.latest.status != apitype.StatusRunning
				server.mu.Unlock()
				if finished {
					continue
				}
				if err := server.Save(); err != nil {
					t.Error(err)
				}
				server.mu.Lock()
				if server.stats.saves > before {
					mu.Lock()
					written++
					mu.Unlock()
				}
				server.mu.Unlock()
			}
		}
	}()
	run(t, dir, cli, slices.Concat(env, server.Env()),
		"up", "--yes", "-f", "--non-interactive", "--stack", "organization/bench/dev")
	close(stop)
	<-done
	server.Close()
	return server, written
}

// An update where every step is a `same` one writes the state once, when it
// ends, as Pulumi's own backend does, instead of after every batch of steps.
func TestUnchangedStepsDoNotSave(t *testing.T) {
	cli, env := pulumiEnv(t)
	dir := t.TempDir()
	diy := slices.Concat(env, []string{"PULUMI_BACKEND_URL=file://" + dir})
	project(t, dir, 200, func(int) int { return 1 })
	run(t, dir, cli, diy, "stack", "init", "organization/bench/dev")
	up(t, dir, cli, env)
	if err := os.Remove(dir + "/Pulumi.dev.yaml"); err != nil {
		t.Fatal(err)
	}

	server, written := saving(t, dir, cli, env)
	if written != 0 {
		t.Errorf("the state was written %d times while nothing changed, want 0", written)
	}
	if server.stats.saves != 1 {
		t.Errorf("the update ended with %d saves, want the final one only", server.stats.saves)
	}
	// Every entry is a same step's Begin or Success, or the secrets manager the
	// stack already has.
	if server.stats.skipped != server.stats.entries {
		t.Errorf("skipped %d of %d entries, want all of them", server.stats.skipped, server.stats.entries)
	}
	checkResources(t, dir, 200)
	run(t, dir, cli, diy, "preview", "--expect-no-changes", "--non-interactive", "--stack", "organization/bench/dev")
}

// A change among unchanged steps is saved as soon as it is applied.
func TestAChangeAmongUnchangedStepsSaves(t *testing.T) {
	cli, env := pulumiEnv(t)
	dir := t.TempDir()
	diy := slices.Concat(env, []string{"PULUMI_BACKEND_URL=file://" + dir})
	project(t, dir, 200, func(int) int { return 1 })
	run(t, dir, cli, diy, "stack", "init", "organization/bench/dev")
	up(t, dir, cli, env)
	if err := os.Remove(dir + "/Pulumi.dev.yaml"); err != nil {
		t.Fatal(err)
	}

	// One resource of the 200 is replaced.
	project(t, dir, 200, func(i int) int {
		if i == 100 {
			return 2
		}
		return 1
	})
	server, written := saving(t, dir, cli, env)
	if written == 0 {
		t.Error("a replaced resource was never saved before the update ended")
	}
	if server.stats.skipped == 0 {
		t.Error("no entry was skipped, while 199 resources did not change")
	}
	checkResources(t, dir, 200)
	run(t, dir, cli, diy, "preview", "--expect-no-changes", "--non-interactive", "--stack", "organization/bench/dev")
}

func TestKilobytes(t *testing.T) {
	cases := map[int]string{0: "0 KB", 1023: "1 KB", 34490369: "33,682 KB", 43194702: "42,182 KB", 1 << 30: "1,048,576 KB"}
	for n, want := range cases {
		if got := kilobytes(n); got != want {
			t.Errorf("kilobytes(%d) = %q, want %q", n, got, want)
		}
	}
}
