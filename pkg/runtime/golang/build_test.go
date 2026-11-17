package golang

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/sst/sst/v3/pkg/runtime"
)

// testIntegrationEnv returns a minimal, hermetic env for `go list`.
// GOTOOLCHAIN=local prevents network downloads of mismatched
// toolchains, GOPROXY=off prevents module fetches, GOMODCACHE/GOCACHE
// are redirected so the host caches stay untouched. GOWORK=off keeps
// the test isolated from any go.work file the host may have.
func testIntegrationEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	disableTelemetry(t, home)
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GOMODCACHE=" + filepath.Join(t.TempDir(), "modcache"),
		"GOCACHE=" + filepath.Join(t.TempDir(), "gocache"),
		"GOPROXY=off",
		"GOFLAGS=-mod=mod",
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"GOSUMDB=off",
	}
}

// disableTelemetry turns Go telemetry off for a toolchain run rooted at the
// given HOME.
//
// The go command writes counter files under the user config directory while it
// works, and t.TempDir removes that directory as soon as the test ends: the two
// race, and the cleanup fails the test with "directory not empty". Nothing here
// wants telemetry anyway.
//
// It has to be the mode file. GOTELEMETRY is reported by `go env` but is not
// read back from the environment, and GOTELEMETRYDIR does not move the counters
// either -- both were tried, and the counters still landed under HOME.
func disableTelemetry(t *testing.T, home string) {
	t.Helper()
	// Where os.UserConfigDir would land for a process with this HOME.
	config := filepath.Join(home, ".config")
	if goruntime.GOOS == "darwin" {
		config = filepath.Join(home, "Library", "Application Support")
	}
	dir := filepath.Join(config, "go", "telemetry")
	mustMkdirAll(t, dir)
	mustWriteFile(t, filepath.Join(dir, "mode"), "off\n")
}

// requireGoToolchain skips when `go test -short` is set or when the
// `go` binary is not in PATH.
func requireGoToolchain(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not available: %v", err)
	}
}

func TestCaptureDeps_SimpleMain(t *testing.T) {
	requireGoToolchain(t)

	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "go.mod"), "module example.test\n\ngo 1.22\n")
	main := filepath.Join(dir, "main.go")
	mustWriteFile(t, main, "package main\n\nfunc main() {}\n")

	r := New()
	deps, err := r.captureDeps(context.Background(), dir, ".", testIntegrationEnv(t))
	if err != nil {
		t.Fatalf("captureDeps: %v", err)
	}
	files := deps.files

	if _, ok := files[main]; !ok {
		t.Errorf("expected main.go (%s) in captured files, got %v", main, keys(files))
	}
	for f := range files {
		if !strings.HasPrefix(f, dir) {
			t.Errorf("captured file outside module root: %s (root=%s)", f, dir)
		}
	}
}

func TestCaptureDeps_LocalDependency(t *testing.T) {
	requireGoToolchain(t)

	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "go.mod"), "module example.test\n\ngo 1.22\n")

	mustMkdirAll(t, filepath.Join(dir, "shared"))
	sharedFile := filepath.Join(dir, "shared", "lib.go")
	mustWriteFile(t, sharedFile, "package shared\n\nfunc Hello() string { return \"hi\" }\n")

	mainFile := filepath.Join(dir, "main.go")
	mustWriteFile(t, mainFile,
		"package main\n\nimport \"example.test/shared\"\n\nfunc main() { _ = shared.Hello() }\n")

	r := New()
	deps, err := r.captureDeps(context.Background(), dir, ".", testIntegrationEnv(t))
	if err != nil {
		t.Fatalf("captureDeps: %v", err)
	}
	files := deps.files

	if _, ok := files[mainFile]; !ok {
		t.Errorf("expected main.go (%s) in captured files, got %v", mainFile, keys(files))
	}
	if _, ok := files[sharedFile]; !ok {
		t.Errorf("expected shared/lib.go (%s) in captured files, got %v", sharedFile, keys(files))
	}
}

func TestCaptureDeps_NoFilesOutsideModuleRoot(t *testing.T) {
	requireGoToolchain(t)

	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "go.mod"), "module example.test\n\ngo 1.22\n")
	mustWriteFile(t, filepath.Join(dir, "main.go"),
		"package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hi\") }\n")

	r := New()
	deps, err := r.captureDeps(context.Background(), dir, ".", testIntegrationEnv(t))
	if err != nil {
		t.Fatalf("captureDeps: %v", err)
	}
	files := deps.files

	for f := range files {
		if !strings.HasPrefix(f, dir) {
			t.Errorf("file outside module root leaked into captured set: %s", f)
		}
	}
}

func TestCaptureDeps_NoTestFilesIncluded(t *testing.T) {
	requireGoToolchain(t)

	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "go.mod"), "module example.test\n\ngo 1.22\n")
	mustWriteFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	testFile := filepath.Join(dir, "main_test.go")
	mustWriteFile(t, testFile, "package main\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n")

	r := New()
	deps, err := r.captureDeps(context.Background(), dir, ".", testIntegrationEnv(t))
	if err != nil {
		t.Fatalf("captureDeps: %v", err)
	}
	files := deps.files

	if _, ok := files[testFile]; ok {
		t.Errorf("test file %s leaked into captured set", testFile)
	}
}

func TestCaptureDeps_BrokenSourceFails(t *testing.T) {
	requireGoToolchain(t)

	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "go.mod"), "module example.test\n\ngo 1.22\n")
	mustWriteFile(t, filepath.Join(dir, "main.go"), "this is not valid go source")

	r := New()
	if _, err := r.captureDeps(context.Background(), dir, ".", testIntegrationEnv(t)); err == nil {
		t.Error("expected error on unparseable source, got nil")
	}
}

func TestCaptureDeps_SubPackageHandler(t *testing.T) {
	requireGoToolchain(t)

	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "go.mod"), "module example.test\n\ngo 1.22\n")

	mustMkdirAll(t, filepath.Join(dir, "shared"))
	sharedFile := filepath.Join(dir, "shared", "lib.go")
	mustWriteFile(t, sharedFile, "package shared\n\nfunc Hello() string { return \"hi\" }\n")

	mustMkdirAll(t, filepath.Join(dir, "lambdas", "alpha"))
	alphaFile := filepath.Join(dir, "lambdas", "alpha", "main.go")
	mustWriteFile(t, alphaFile,
		"package main\n\nimport \"example.test/shared\"\n\nfunc main() { _ = shared.Hello() }\n")

	mustMkdirAll(t, filepath.Join(dir, "lambdas", "beta"))
	betaFile := filepath.Join(dir, "lambdas", "beta", "main.go")
	mustWriteFile(t, betaFile, "package main\n\nfunc main() {}\n")

	r := New()
	deps, err := r.captureDeps(context.Background(), dir, "lambdas/alpha", testIntegrationEnv(t))
	if err != nil {
		t.Fatalf("captureDeps: %v", err)
	}
	files := deps.files

	if _, ok := files[alphaFile]; !ok {
		t.Errorf("expected alpha main.go in captured files, got %v", keys(files))
	}
	if _, ok := files[sharedFile]; !ok {
		t.Errorf("expected shared/lib.go in captured files, got %v", keys(files))
	}
	if _, ok := files[betaFile]; ok {
		t.Errorf("beta sibling main.go must NOT be in alpha's import graph: %s", betaFile)
	}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildBootstrap runs a deploy-mode Build (Dev false, the branch that carries
// the linker flags) and returns the artifact bytes. Each call uses its own
// functionID, so the output path is fresh every time, the way a deploy's is.
func buildBootstrap(t *testing.T, cfgDir, handler, functionID string) []byte {
	t.Helper()

	input := &runtime.BuildInput{
		CfgPath:    filepath.Join(cfgDir, "sst.config.ts"),
		FunctionID: functionID,
		Handler:    handler,
	}
	if err := os.MkdirAll(input.Out(), 0o755); err != nil {
		t.Fatalf("mkdir out: %v", err)
	}

	out, err := New().Build(context.Background(), input)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(out.Errors) > 0 {
		t.Fatalf("Build reported errors: %v", out.Errors)
	}

	bin, err := os.ReadFile(filepath.Join(input.Out(), "bootstrap"))
	if err != nil {
		t.Fatalf("read bootstrap: %v", err)
	}
	return bin
}

// The Go build ID note hashes every source file that fed the build, the ones
// the linker discards as unreachable included. In a monorepo where a single
// package is imported by hundreds of handlers that is the whole difference
// between a deploy that updates one Function and a deploy that re-uploads all
// of them: the note changes everywhere, the linked code changes nowhere.
//
// So the assertion is the property, not the flag. Asserting that the args
// contain "-buildid=" would pass just as happily if a Go release changed what
// the flag does.
func TestBuild_UnreachableSourceChangeKeepsArtifactIdentical(t *testing.T) {
	requireGoToolchain(t)

	cfgDir := t.TempDir()
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "go.mod"), "module example.test\n\ngo 1.22\n")
	mustWriteFile(t, filepath.Join(dir, "main.go"),
		"package main\n\nimport \"example.test/lib\"\n\nfunc main() { println(lib.Used()) }\n")
	mustMkdirAll(t, filepath.Join(dir, "lib"))
	used := filepath.Join(dir, "lib", "used.go")
	unused := filepath.Join(dir, "lib", "unused.go")
	mustWriteFile(t, used, "package lib\n\nfunc Used() string { return \"used\" }\n")
	mustWriteFile(t, unused, "package lib\n\nfunc Unused() string { return \"before\" }\n")

	first := buildBootstrap(t, cfgDir, dir, "fn-first")

	// Same package, a function nothing calls. The linker drops it, so the
	// program is byte-for-byte the same and the artifact has to be too.
	mustWriteFile(t, unused, "package lib\n\nfunc Unused() string { return \"after\" }\n")
	second := buildBootstrap(t, cfgDir, dir, "fn-second")

	if !bytes.Equal(first, second) {
		t.Errorf("editing an unreachable file changed the artifact (%d vs %d bytes): "+
			"every handler importing this package would redeploy for nothing",
			len(first), len(second))
	}

	// Guard against a vacuous pass: if the two above matched because the build
	// stopped distinguishing anything at all, this catches it.
	mustWriteFile(t, used, "package lib\n\nfunc Used() string { return \"changed\" }\n")
	third := buildBootstrap(t, cfgDir, dir, "fn-third")

	if bytes.Equal(first, third) {
		t.Error("editing reachable code left the artifact unchanged: " +
			"a real change would not be deployed")
	}
}
