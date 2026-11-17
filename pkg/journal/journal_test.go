package journal

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/sst/sst/v3/pkg/global"
)

// TestMain shows what the backend logs when the tests run with -v.
func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Verbose() {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	}
	os.Exit(m.Run())
}

// The provider the end-to-end tests deploy. It creates its resources locally,
// so they run without a cloud account, but only where SST already installed it.
const randomPlugin = "resource-random-v4.21.4"

// pulumiEnv finds the Pulumi CLI SST installs and the random provider, and
// gives them a home of their own. It skips the test when either is missing.
func pulumiEnv(t *testing.T) (string, []string) {
	t.Helper()
	cli := global.PulumiPath()
	if _, err := os.Stat(cli); err != nil {
		t.Skipf("no Pulumi CLI at %s", cli)
	}
	plugin := filepath.Join(global.ConfigDir(), "plugins", randomPlugin)
	if _, err := os.Stat(plugin); err != nil {
		t.Skipf("no %s plugin at %s", randomPlugin, plugin)
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "plugins"), 0755); err != nil {
		t.Fatal(err)
	}
	// A copy: Pulumi skips a plugin directory that is a symlink.
	if err := os.CopyFS(filepath.Join(home, "plugins", randomPlugin), os.DirFS(plugin)); err != nil {
		t.Fatal(err)
	}
	return cli, append(os.Environ(),
		"PULUMI_HOME="+home,
		"PULUMI_CONFIG_PASSPHRASE=test",
		"PULUMI_SKIP_UPDATE_CHECK=true",
		"PULUMI_IGNORE_AMBIENT_PLUGINS=true",
	)
}

// project writes a Pulumi YAML program with n random strings, each one
// replaced whenever its generation changes, padded to bulk up the state.
func project(t *testing.T, dir string, n int, generation func(i int) int) {
	t.Helper()
	var b strings.Builder
	b.WriteString("name: bench\nruntime: yaml\n")
	fmt.Fprintf(&b, "variables:\n  pad: %s\n", strings.Repeat("x", 512))
	b.WriteString("resources:\n")
	for i := range n {
		fmt.Fprintf(&b, "  r%d:\n    type: random:RandomString\n    properties:\n      length: 8\n", i)
		fmt.Fprintf(&b, "      keepers:\n        pad: ${pad}\n        gen: \"%d\"\n", generation(i))
		b.WriteString("    options:\n      version: 4.21.4\n")
	}
	// A secret, so the state has something the passphrase encrypts.
	b.WriteString("  secret:\n    type: random:RandomPassword\n    properties:\n      length: 16\n")
	b.WriteString("    options:\n      version: 4.21.4\n")
	if err := os.WriteFile(filepath.Join(dir, "Pulumi.yaml"), []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, dir, cli string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(cli, args...)
	cmd.Dir = dir
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("pulumi %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

func statePath(dir string) string {
	return filepath.Join(dir, ".pulumi", "stacks", "bench", "dev.json")
}

// up runs `pulumi up` through a journaling backend on the stack's state file.
func up(t *testing.T, dir, cli string, env []string) *Server {
	t.Helper()
	server, err := Start(Options{
		StatePath: statePath(dir),
		Org:       "organization",
		Project:   "bench",
		Stack:     "dev",
		Dir:       dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	run(t, dir, cli, slices.Concat(env, server.Env()),
		"up", "--yes", "-f", "--non-interactive", "--stack", "organization/bench/dev")
	if err := server.Save(); err != nil {
		t.Fatal(err)
	}
	server.Close()
	return server
}

// TestUpThenPreviewOnTheFile deploys through the journal, creates and
// replaces, and then hands the state to Pulumi's own DIY backend: if the file
// the journal wrote were not the stack as deployed, its preview would show
// changes.
func TestUpThenPreviewOnTheFile(t *testing.T) {
	cli, env := pulumiEnv(t)
	dir := t.TempDir()
	diy := slices.Concat(env, []string{"PULUMI_BACKEND_URL=file://" + dir})
	project(t, dir, 20, func(int) int { return 1 })
	run(t, dir, cli, diy, "stack", "init", "organization/bench/dev")

	up(t, dir, cli, env)
	checkResources(t, dir, 20)
	run(t, dir, cli, diy, "preview", "--expect-no-changes", "--non-interactive", "--stack", "organization/bench/dev")

	// Replace half of them: a create, a replace and a delete each. Without the
	// stack's config file, as SST runs every update in a new directory: the
	// secrets manager has to come from the state.
	if err := os.Remove(filepath.Join(dir, "Pulumi.dev.yaml")); err != nil {
		t.Fatal(err)
	}
	project(t, dir, 20, func(i int) int { return 1 + i%2 })
	server := up(t, dir, cli, env)
	if server.stats.entries == 0 {
		t.Fatal("the update sent no journal entries")
	}
	checkResources(t, dir, 20)
	run(t, dir, cli, diy, "preview", "--expect-no-changes", "--non-interactive", "--stack", "organization/bench/dev")
}

func checkResources(t *testing.T, dir string, want int) {
	t.Helper()
	data, err := os.ReadFile(statePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var versioned apitype.VersionedCheckpoint
	if err := json.Unmarshal(data, &versioned); err != nil {
		t.Fatal(err)
	}
	var checkpoint apitype.CheckpointV3
	if err := json.Unmarshal(versioned.Checkpoint, &checkpoint); err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, res := range checkpoint.Latest.Resources {
		if res.Type == "random:index/randomString:RandomString" {
			got++
			if res.Delete {
				t.Errorf("%s is left marked for deletion", res.URN)
			}
		}
	}
	if got != want {
		t.Fatalf("got %d random strings in the state, want %d", got, want)
	}
	if n := len(checkpoint.Latest.PendingOperations); n > 0 {
		t.Fatalf("%d pending operations left in the state", n)
	}
	if sp := checkpoint.Latest.SecretsProviders; sp == nil || sp.Type != "passphrase" {
		t.Fatalf("the state's secrets provider is %+v, want the passphrase", sp)
	}
}
