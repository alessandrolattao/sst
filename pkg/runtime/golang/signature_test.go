package golang

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sst/sst/v3/pkg/runtime"
)

// The fixture is a small monorepo: handlers that share packages, directly and
// through other packages, and a handler and a package that share nothing.
//
//	handlers/a  -> shared/x
//	handlers/b  -> shared/y -> shared/z
//	handlers/ab -> shared/x, shared/y -> shared/z
//	handlers/c  -> (standard library only)
//	shared/lonely is imported by nobody
var signFixture = map[string]string{
	"go.mod":                  "module example.test\n\ngo 1.22\n",
	"handlers/a/main.go":      "package main\n\nimport \"example.test/shared/x\"\n\nfunc main() { println(x.Name()) }\n",
	"handlers/b/main.go":      "package main\n\nimport \"example.test/shared/y\"\n\nfunc main() { println(y.Name()) }\n",
	"handlers/ab/main.go":     "package main\n\nimport (\n\t\"example.test/shared/x\"\n\t\"example.test/shared/y\"\n)\n\nfunc main() { println(x.Name(), y.Name()) }\n",
	"handlers/c/main.go":      "package main\n\nimport \"strings\"\n\nfunc main() { println(strings.ToUpper(\"c\")) }\n",
	"shared/x/x.go":           "package x\n\nfunc Name() string { return \"x\" }\n",
	"shared/y/y.go":           "package y\n\nimport \"example.test/shared/z\"\n\nfunc Name() string { return \"y\" + z.Name() }\n",
	"shared/z/z.go":           "package z\n\nfunc Name() string { return \"z\" }\n",
	"shared/lonely/lonely.go": "package lonely\n\nfunc Name() string { return \"lonely\" }\n",
}

var signFixtureHandlers = []string{"handlers/a", "handlers/b", "handlers/ab", "handlers/c"}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		mustMkdirAll(t, filepath.Dir(path))
		mustWriteFile(t, path, contents)
	}
}

func newSignFixture(t *testing.T) string {
	t.Helper()
	requireGoToolchain(t)
	// the go command reports directories with their links resolved
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, root, signFixture)
	return root
}

// signEnv is the environment of a deploy build, kept off the network and away
// from any workspace of the machine running the tests.
func signEnv(architecture string, extra ...string) []string {
	env := append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off", "GOPROXY=off")
	env = append(env, extra...)
	return deployEnv(env, architecture)
}

// signAll signs the module and returns the signatures by handler path
// relative to the root, "" for a handler that could not be signed.
func signAll(t *testing.T, root string, env []string, handlers []string) map[string]string {
	t.Helper()
	byDir, err := signModule(context.Background(), root, env)
	if err != nil {
		t.Fatalf("signModule: %v", err)
	}
	result := map[string]string{}
	for _, handler := range handlers {
		result[handler] = byDir[filepath.Join(root, filepath.FromSlash(handler))]
	}
	return result
}

func requireSigned(t *testing.T, signatures map[string]string) {
	t.Helper()
	for handler, signature := range signatures {
		if signature == "" {
			t.Fatalf("%s has no signature", handler)
		}
	}
}

// expectChanged asserts that exactly the handlers in want changed signature.
func expectChanged(t *testing.T, before, after map[string]string, want ...string) {
	t.Helper()
	wanted := map[string]bool{}
	for _, handler := range want {
		wanted[handler] = true
	}
	for handler := range before {
		changed := before[handler] != after[handler]
		if changed && !wanted[handler] {
			t.Errorf("%s changed signature, and nothing it imports changed: it would be rebuilt for nothing", handler)
		}
		if !changed && wanted[handler] {
			t.Errorf("%s kept its signature across a change to what it imports: the change would not be deployed", handler)
		}
	}
}

func TestSign_SameSourcesSameSignature(t *testing.T) {
	root := newSignFixture(t)
	env := signEnv("arm64")
	first := signAll(t, root, env, signFixtureHandlers)
	requireSigned(t, first)
	second := signAll(t, root, env, signFixtureHandlers)
	expectChanged(t, first, second)

	seen := map[string]string{}
	for handler, signature := range first {
		if other, ok := seen[signature]; ok {
			t.Errorf("%s and %s have the same signature", handler, other)
		}
		seen[signature] = handler
	}
}

// The same sources in another directory are the same build once -trimpath
// takes the paths out, which is how a deploy on a fresh CI checkout runs.
func TestSign_SameSourcesElsewhereSameSignature(t *testing.T) {
	first := newSignFixture(t)
	second := newSignFixture(t)
	env := signEnv("arm64", "GOFLAGS=-mod=mod -trimpath")
	expectChanged(t,
		signAll(t, first, env, signFixtureHandlers),
		signAll(t, second, env, signFixtureHandlers),
	)
}

func TestSign_Changes(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, root string)
		want   []string
	}{
		{
			name: "the handler's own file",
			change: func(t *testing.T, root string) {
				mustWriteFile(t, filepath.Join(root, "handlers/a/main.go"),
					"package main\n\nimport \"example.test/shared/x\"\n\nfunc main() { println(x.Name(), \"!\") }\n")
			},
			want: []string{"handlers/a"},
		},
		{
			name: "a new file in the handler",
			change: func(t *testing.T, root string) {
				mustWriteFile(t, filepath.Join(root, "handlers/c/extra.go"),
					"package main\n\nfunc init() { println(\"extra\") }\n")
			},
			want: []string{"handlers/c"},
		},
		{
			name: "a function body in a package imported directly",
			change: func(t *testing.T, root string) {
				mustWriteFile(t, filepath.Join(root, "shared/x/x.go"),
					"package x\n\nfunc Name() string { return \"x2\" }\n")
			},
			want: []string{"handlers/a", "handlers/ab"},
		},
		{
			name: "a function body in a package imported through another",
			change: func(t *testing.T, root string) {
				mustWriteFile(t, filepath.Join(root, "shared/z/z.go"),
					"package z\n\nfunc Name() string { return \"z2\" }\n")
			},
			want: []string{"handlers/b", "handlers/ab"},
		},
		{
			name: "a constant in a shared package",
			change: func(t *testing.T, root string) {
				mustWriteFile(t, filepath.Join(root, "shared/z/z.go"),
					"package z\n\nconst name = \"zz\"\n\nfunc Name() string { return name }\n")
			},
			want: []string{"handlers/b", "handlers/ab"},
		},
		{
			name: "a new file in a shared package",
			change: func(t *testing.T, root string) {
				mustWriteFile(t, filepath.Join(root, "shared/x/more.go"),
					"package x\n\nfunc init() { println(\"more\") }\n")
			},
			want: []string{"handlers/a", "handlers/ab"},
		},
		{
			name: "a file removed from a shared package",
			change: func(t *testing.T, root string) {
				mustWriteFile(t, filepath.Join(root, "shared/y/y.go"),
					"package y\n\nfunc Name() string { return \"y\" }\n")
			},
			want: []string{"handlers/b", "handlers/ab"},
		},
		{
			name: "a shared package starts importing another",
			change: func(t *testing.T, root string) {
				mustWriteFile(t, filepath.Join(root, "shared/x/x.go"),
					"package x\n\nimport \"example.test/shared/lonely\"\n\nfunc Name() string { return \"x\" + lonely.Name() }\n")
			},
			want: []string{"handlers/a", "handlers/ab"},
		},
		{
			name: "a handler starts importing a shared package",
			change: func(t *testing.T, root string) {
				mustWriteFile(t, filepath.Join(root, "handlers/c/main.go"),
					"package main\n\nimport \"example.test/shared/x\"\n\nfunc main() { println(x.Name()) }\n")
			},
			want: []string{"handlers/c"},
		},
		{
			name: "a type in a shared package",
			change: func(t *testing.T, root string) {
				mustWriteFile(t, filepath.Join(root, "shared/x/x.go"),
					"package x\n\ntype T struct{ A, B int }\n\nfunc Name() string { return \"x\" }\n\nfunc New() T { return T{1, 2} }\n")
			},
			want: []string{"handlers/a", "handlers/ab"},
		},
		{
			name: "a package nobody imports",
			change: func(t *testing.T, root string) {
				mustWriteFile(t, filepath.Join(root, "shared/lonely/lonely.go"),
					"package lonely\n\nfunc Name() string { return \"changed\" }\n")
			},
		},
		{
			name: "a new package nobody imports",
			change: func(t *testing.T, root string) {
				writeTree(t, root, map[string]string{"shared/fresh/fresh.go": "package fresh\n\nfunc Name() string { return \"fresh\" }\n"})
			},
		},
		{
			name: "a new handler",
			change: func(t *testing.T, root string) {
				writeTree(t, root, map[string]string{"handlers/d/main.go": "package main\n\nimport \"example.test/shared/x\"\n\nfunc main() { println(x.Name()) }\n"})
			},
		},
		{
			name: "a test file in a shared package",
			change: func(t *testing.T, root string) {
				mustWriteFile(t, filepath.Join(root, "shared/x/x_test.go"),
					"package x\n\nimport \"testing\"\n\nfunc TestName(t *testing.T) {}\n")
			},
		},
		{
			name: "a file for another operating system",
			change: func(t *testing.T, root string) {
				mustWriteFile(t, filepath.Join(root, "shared/x/x_windows.go"),
					"package x\n\nfunc init() { println(\"windows\") }\n")
			},
		},
		{
			name: "a file that is not Go",
			change: func(t *testing.T, root string) {
				mustWriteFile(t, filepath.Join(root, "shared/x/README.md"), "notes\n")
			},
		},
		{
			name: "the module file",
			change: func(t *testing.T, root string) {
				mustWriteFile(t, filepath.Join(root, "go.mod"), "module example.test\n\ngo 1.23\n")
			},
			want: signFixtureHandlers,
		},
		{
			name: "the module checksums",
			change: func(t *testing.T, root string) {
				mustWriteFile(t, filepath.Join(root, "go.sum"), "example.invalid v1.0.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n")
			},
			want: signFixtureHandlers,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := newSignFixture(t)
			env := signEnv("arm64")
			before := signAll(t, root, env, signFixtureHandlers)
			requireSigned(t, before)
			tc.change(t, root)
			after := signAll(t, root, env, signFixtureHandlers)
			requireSigned(t, after)
			expectChanged(t, before, after, tc.want...)
		})
	}
}

// Undoing a change gives back the signature the sources had before, so a
// revert finds the zip that is deployed only when it really is that one.
func TestSign_RevertGivesBackTheSignature(t *testing.T) {
	root := newSignFixture(t)
	env := signEnv("arm64")
	before := signAll(t, root, env, signFixtureHandlers)
	mustWriteFile(t, filepath.Join(root, "shared/z/z.go"), "package z\n\nfunc Name() string { return \"other\" }\n")
	changed := signAll(t, root, env, signFixtureHandlers)
	expectChanged(t, before, changed, "handlers/b", "handlers/ab")
	mustWriteFile(t, filepath.Join(root, "shared/z/z.go"), signFixture["shared/z/z.go"])
	expectChanged(t, before, signAll(t, root, env, signFixtureHandlers))
}

func TestSign_EmbeddedFile(t *testing.T) {
	root := newSignFixture(t)
	writeTree(t, root, map[string]string{
		"shared/x/x.go":     "package x\n\nimport _ \"embed\"\n\n//go:embed data.txt\nvar data string\n\nfunc Name() string { return data }\n",
		"shared/x/data.txt": "one\n",
	})
	env := signEnv("arm64")
	before := signAll(t, root, env, signFixtureHandlers)
	requireSigned(t, before)
	mustWriteFile(t, filepath.Join(root, "shared/x/data.txt"), "two\n")
	expectChanged(t, before, signAll(t, root, env, signFixtureHandlers), "handlers/a", "handlers/ab")
}

func TestSign_BuildTag(t *testing.T) {
	root := newSignFixture(t)
	writeTree(t, root, map[string]string{
		"shared/x/x.go":      "//go:build !special\n\npackage x\n\nfunc Name() string { return \"x\" }\n",
		"shared/x/x_spec.go": "//go:build special\n\npackage x\n\nfunc Name() string { return \"special\" }\n",
	})
	plain := signAll(t, root, signEnv("arm64"), signFixtureHandlers)
	requireSigned(t, plain)
	tagged := signAll(t, root, signEnv("arm64", "GOFLAGS=-mod=mod -tags=special"), signFixtureHandlers)
	requireSigned(t, tagged)
	// the flags are part of every signature, whatever the tag selects
	expectChanged(t, plain, tagged, signFixtureHandlers...)

	// the file the tag leaves out is not part of the build
	mustWriteFile(t, filepath.Join(root, "shared/x/x_spec.go"), "//go:build special\n\npackage x\n\nfunc Name() string { return \"special2\" }\n")
	expectChanged(t, plain, signAll(t, root, signEnv("arm64"), signFixtureHandlers))
	expectChanged(t, tagged, signAll(t, root, signEnv("arm64", "GOFLAGS=-mod=mod -tags=special"), signFixtureHandlers),
		"handlers/a", "handlers/ab")
}

func TestSign_Architecture(t *testing.T) {
	root := newSignFixture(t)
	arm := signAll(t, root, signEnv("arm64"), signFixtureHandlers)
	requireSigned(t, arm)
	x86 := signAll(t, root, signEnv("x86_64"), signFixtureHandlers)
	requireSigned(t, x86)
	expectChanged(t, arm, x86, signFixtureHandlers...)
}

// Shared code that lives in another module, reached through a replace.
func TestSign_ReplacedModule(t *testing.T) {
	root := newSignFixture(t)
	other, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, other, map[string]string{
		"go.mod": "module example.test/other\n\ngo 1.22\n",
		"o.go":   "package other\n\nfunc Name() string { return \"other\" }\n",
	})
	writeTree(t, root, map[string]string{
		"go.mod":        "module example.test\n\ngo 1.22\n\nrequire example.test/other v0.0.0\n\nreplace example.test/other => " + other + "\n",
		"shared/x/x.go": "package x\n\nimport \"example.test/other\"\n\nfunc Name() string { return other.Name() }\n",
	})
	env := signEnv("arm64")
	before := signAll(t, root, env, signFixtureHandlers)
	requireSigned(t, before)
	mustWriteFile(t, filepath.Join(other, "o.go"), "package other\n\nfunc Name() string { return \"other2\" }\n")
	expectChanged(t, before, signAll(t, root, env, signFixtureHandlers), "handlers/a", "handlers/ab")
}

// Shared code that lives in another module of the same workspace.
func TestSign_Workspace(t *testing.T) {
	requireGoToolchain(t)
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(work, "app")
	writeTree(t, root, signFixture)
	writeTree(t, work, map[string]string{
		"go.work":           "go 1.22\n\nuse (\n\t./app\n\t./lib\n)\n",
		"lib/go.mod":        "module example.test/lib\n\ngo 1.22\n",
		"lib/l.go":          "package lib\n\nfunc Name() string { return \"lib\" }\n",
		"app/shared/x/x.go": "package x\n\nimport \"example.test/lib\"\n\nfunc Name() string { return lib.Name() }\n",
	})
	env := deployEnv(append(os.Environ(), "GOFLAGS=", "GOPROXY=off", "GOWORK="+filepath.Join(work, "go.work")), "arm64")
	before := signAll(t, root, env, signFixtureHandlers)
	requireSigned(t, before)

	mustWriteFile(t, filepath.Join(work, "lib/l.go"), "package lib\n\nfunc Name() string { return \"lib2\" }\n")
	changed := signAll(t, root, env, signFixtureHandlers)
	expectChanged(t, before, changed, "handlers/a", "handlers/ab")

	// the workspace file decides which modules a build sees
	mustWriteFile(t, filepath.Join(work, "go.work"), "go 1.23\n\nuse (\n\t./app\n\t./lib\n)\n")
	expectChanged(t, changed, signAll(t, root, env, signFixtureHandlers), signFixtureHandlers...)
}

// A package that does not compile takes the signature away from the handlers
// that import it, so they are built and the build reports the error. The
// others keep theirs.
func TestSign_BrokenPackage(t *testing.T) {
	root := newSignFixture(t)
	env := signEnv("arm64")
	before := signAll(t, root, env, signFixtureHandlers)
	requireSigned(t, before)

	mustWriteFile(t, filepath.Join(root, "shared/z/z.go"), "package z\n\nfunc Name() string { return 1 }\n")
	after := signAll(t, root, env, signFixtureHandlers)
	for _, handler := range []string{"handlers/b", "handlers/ab"} {
		if after[handler] != "" {
			t.Errorf("%s imports a package that does not compile and still has a signature", handler)
		}
	}
	for _, handler := range []string{"handlers/a", "handlers/c"} {
		if after[handler] != before[handler] {
			t.Errorf("%s lost or changed its signature over a package it does not import", handler)
		}
	}
}

func TestSign_BrokenHandlerAndMissingImport(t *testing.T) {
	root := newSignFixture(t)
	env := signEnv("arm64")
	before := signAll(t, root, env, signFixtureHandlers)

	mustWriteFile(t, filepath.Join(root, "handlers/a/main.go"), "package main\n\nfunc main() { undefined() }\n")
	mustWriteFile(t, filepath.Join(root, "handlers/c/main.go"), "package main\n\nimport \"example.test/shared/nowhere\"\n\nfunc main() { nowhere.Name() }\n")
	after := signAll(t, root, env, signFixtureHandlers)
	if after["handlers/a"] != "" {
		t.Error("a handler that does not compile has a signature")
	}
	if after["handlers/c"] != "" {
		t.Error("a handler with an import that does not exist has a signature")
	}
	for _, handler := range []string{"handlers/b", "handlers/ab"} {
		if after[handler] != before[handler] {
			t.Errorf("%s lost or changed its signature over another handler's error", handler)
		}
	}
}

// What the whole thing rests on: the same signature means the same binary.
// Every variant of the sources is signed and built, and no two variants may
// share a signature unless they also share the binary, for every handler.
func TestSign_SameSignatureSameBinary(t *testing.T) {
	root := newSignFixture(t)
	cfgDir := t.TempDir()
	t.Setenv("GOFLAGS", "-mod=mod -trimpath")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")
	env := deployEnv(os.Environ(), "arm64")

	variants := []map[string]string{
		{},
		{"shared/x/x.go": "package x\n\nfunc Name() string { return \"x2\" }\n"},
		{"shared/z/z.go": "package z\n\nfunc Name() string { return \"z2\" }\n"},
		// reachable by nobody: the binary stays, the signature may not
		{"shared/x/unused.go": "package x\n\nfunc Unused() string { return \"unused\" }\n"},
		{"shared/x/x.go": "package x\n\n// a comment\nfunc Name() string { return \"x\" }\n"},
		{"shared/lonely/lonely.go": "package lonely\n\nfunc Name() string { return \"changed\" }\n"},
		{"handlers/c/main.go": "package main\n\nimport \"strings\"\n\nfunc main() { println(strings.ToLower(\"C\")) }\n"},
		// back to the first one
		{},
	}

	type built struct {
		variant   int
		signature string
		binary    []byte
	}
	seen := map[string][]built{}
	for i, variant := range variants {
		writeTree(t, root, signFixture)
		os.Remove(filepath.Join(root, "shared/x/unused.go"))
		writeTree(t, root, variant)
		signatures := signAll(t, root, env, signFixtureHandlers)
		requireSigned(t, signatures)
		for _, handler := range signFixtureHandlers {
			binary := buildBootstrap(t, cfgDir, filepath.Join(root, filepath.FromSlash(handler)),
				strings.ReplaceAll(handler, "/", "-")+"-"+string(rune('a'+i)))
			for _, other := range seen[handler] {
				sameSignature := other.signature == signatures[handler]
				sameBinary := bytes.Equal(other.binary, binary)
				if sameSignature && !sameBinary {
					t.Errorf("%s: variants %d and %d have the same signature and different binaries: "+
						"the second would never be deployed", handler, other.variant, i)
				}
			}
			seen[handler] = append(seen[handler], built{i, signatures[handler], binary})
		}
	}

	// and the signature is not so nervous that it never matches
	for _, handler := range signFixtureHandlers {
		first, last := seen[handler][0], seen[handler][len(variants)-1]
		if first.signature != last.signature {
			t.Errorf("%s: the same sources signed differently the second time", handler)
		}
	}
	if seen["handlers/c"][0].signature != seen["handlers/c"][1].signature {
		t.Error("handlers/c changed signature over a package it does not import")
	}
}

func TestSign_ThroughTheRuntime(t *testing.T) {
	root := newSignFixture(t)
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")

	properties, _ := json.Marshal(Properties{Architecture: "arm64"})
	input := func(handler string) *runtime.BuildInput {
		return &runtime.BuildInput{Handler: filepath.Join(root, filepath.FromSlash(handler)), Properties: properties}
	}

	r := New()
	a, handler, err := r.Sign(context.Background(), input("handlers/a"))
	if err != nil || a == "" {
		t.Fatalf("Sign = %q, %v", a, err)
	}
	if handler != "bootstrap" {
		t.Errorf("handler = %q, want the one Build reports", handler)
	}
	b, _, err := r.Sign(context.Background(), input("handlers/b"))
	if err != nil || b == "" || b == a {
		t.Fatalf("second handler: Sign = %q, %v (first was %q)", b, err, a)
	}

	// a directory that is not a main package of the module has no signature
	lib, _, err := r.Sign(context.Background(), input("shared/x"))
	if err != nil || lib != "" {
		t.Errorf("a library package: Sign = %q, %v, want no signature", lib, err)
	}

	// dev builds are never signed
	dev := input("handlers/a")
	dev.Dev = true
	if signature, _, _ := r.Sign(context.Background(), dev); signature != "" {
		t.Errorf("a dev build has a signature: %q", signature)
	}

	// One module is signed once per deploy: the same deploy does not see a
	// later change. The next deploy of the same process does.
	mustWriteFile(t, filepath.Join(root, "shared/x/x.go"), "package x\n\nfunc Name() string { return \"x2\" }\n")
	again, _, _ := r.Sign(context.Background(), input("handlers/a"))
	if again != a {
		t.Error("the same deploy signed the module twice")
	}
	r.ForgetSignatures()
	next, _, _ := r.Sign(context.Background(), input("handlers/a"))
	if next == "" || next == a {
		t.Errorf("the next deploy signed with what the one before read: %q", next)
	}
	fresh, _, _ := New().Sign(context.Background(), input("handlers/a"))
	if fresh != next {
		t.Errorf("a new runtime signs %q, the same one after forgetting %q", fresh, next)
	}

	// through a link to the module
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	linked, _, err := New().Sign(context.Background(), &runtime.BuildInput{
		Handler: filepath.Join(link, "handlers", "a"), Properties: properties,
	})
	if err != nil || linked != fresh {
		t.Errorf("through a link: Sign = %q, %v, want %q", linked, err, fresh)
	}
}

func TestSign_NoModule(t *testing.T) {
	requireGoToolchain(t)
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	signature, _, err := New().Sign(context.Background(), &runtime.BuildInput{Handler: dir})
	if err == nil && signature != "" {
		t.Errorf("a handler outside any module has a signature: %q", signature)
	}
}

func TestSignPackages(t *testing.T) {
	base := []byte("base")
	packages := []signedPackage{
		{ImportPath: "example.test/a", Dir: t.TempDir(), Name: "main", Deps: []string{"example.test/lib", "unsafe"}, BuildID: "a1"},
		{ImportPath: "example.test/lib", Name: "lib", BuildID: "l1"},
		{ImportPath: "unsafe", Name: "unsafe"},
	}
	first := signPackages(base, packages)
	if len(first) != 1 {
		t.Fatalf("signed %d packages, want the one main", len(first))
	}
	dir, _ := realPath(packages[0].Dir)

	change := func(edit func(p []signedPackage)) string {
		copied := make([]signedPackage, len(packages))
		copy(copied, packages)
		edit(copied)
		return signPackages(base, copied)[dir]
	}
	if got := change(func(p []signedPackage) {}); got != first[dir] {
		t.Error("the same packages signed differently")
	}
	if got := change(func(p []signedPackage) { p[1].BuildID = "l2" }); got == first[dir] || got == "" {
		t.Error("a dependency's build ID is not part of the signature")
	}
	if got := change(func(p []signedPackage) { p[0].BuildID = "a2" }); got == first[dir] || got == "" {
		t.Error("the main package's build ID is not part of the signature")
	}
	if got := change(func(p []signedPackage) { p[1].BuildID = "" }); got != "" {
		t.Error("a dependency without a build ID was signed")
	}
	if got := change(func(p []signedPackage) { p[0].Deps = []string{"example.test/gone"} }); got != "" {
		t.Error("a dependency go list did not report was signed")
	}
	if got := change(func(p []signedPackage) { p[0].Incomplete = true }); got != "" {
		t.Error("an incomplete package was signed")
	}
	if got := change(func(p []signedPackage) { p[0].DepsErrors = []any{"boom"} }); got != "" {
		t.Error("a package with errors in its dependencies was signed")
	}
	if got := signPackages([]byte("other"), packages)[dir]; got == first[dir] {
		t.Error("the toolchain and flags are not part of the signature")
	}
	// the order go list reports the dependencies in does not matter
	if got := change(func(p []signedPackage) { p[0].Deps = []string{"unsafe", "example.test/lib"} }); got != first[dir] {
		t.Error("the order of the dependencies changed the signature")
	}
}

// writeDeployedState writes the state a deploy leaves: each function with the
// signature of its build and the code object that zip went to.
func writeDeployedState(t *testing.T, functions map[string][3]string) string {
	t.Helper()
	const stack = "urn:pulumi:prod::app::"
	resources := []map[string]any{}
	for name, f := range functions {
		signature, key, zip := f[0], f[1], f[2]
		resources = append(resources,
			map[string]any{
				"urn":     stack + "sst:aws:Function::" + name,
				"type":    "sst:aws:Function",
				"outputs": map[string]any{"_code": map[string]any{"signature": signature, "key": key}},
			},
			map[string]any{
				"urn":    stack + "sst:aws:Function$aws:s3/bucketObjectv2:BucketObjectv2::" + name + "Code",
				"type":   "aws:s3/bucketObjectv2:BucketObjectv2",
				"parent": stack + "sst:aws:Function::" + name,
				"inputs": map[string]any{"key": key, "source": map[string]any{"hash": "h", "path": zip}},
			},
		)
	}
	checkpoint, _ := json.Marshal(map[string]any{"latest": map[string]any{"resources": resources}})
	state, _ := json.Marshal(map[string]any{"version": 3, "checkpoint": json.RawMessage(checkpoint)})
	path := filepath.Join(t.TempDir(), "state.json")
	mustWriteFile(t, path, string(state))
	return path
}

// The whole path, with the real toolchain: a deploy builds and records the
// signatures, the next one builds only what a change reached.
func TestDeploy_BuildsOnlyWhatChanged(t *testing.T) {
	root := newSignFixture(t)
	t.Setenv("GOFLAGS", "-mod=mod -trimpath")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")
	cfgPath := filepath.Join(t.TempDir(), "sst.config.ts")
	properties, _ := json.Marshal(Properties{Architecture: "arm64"})

	names := map[string]string{"handlers/a": "FnA", "handlers/b": "FnB", "handlers/ab": "FnAB", "handlers/c": "FnC"}
	zip := func(name string) string {
		return filepath.Join(filepath.Dir(cfgPath), ".sst", "artifacts", name, "code.zip")
	}
	key := func(name string, deploy int) string {
		return "assets/" + name + "-code-" + string(rune('0'+deploy)) + ".zip"
	}

	// deploy runs one deploy from the given state and returns, per handler,
	// the build output and the bytes of the binary ("" when not built).
	type result struct {
		out    *runtime.BuildOutput
		binary string
	}
	deploy := func(statePath string) map[string]result {
		t.Helper()
		collection := runtime.NewCollection(cfgPath, New())
		if statePath != "" {
			collection.LoadDeployed(statePath)
		}
		results := map[string]result{}
		for handler, name := range names {
			input := &runtime.BuildInput{
				CfgPath:    cfgPath,
				FunctionID: name,
				Runtime:    "go",
				Handler:    filepath.Join(root, filepath.FromSlash(handler)),
				Properties: properties,
				Code:       zip(name),
			}
			os.RemoveAll(input.Out())
			out, err := collection.Build(context.Background(), input)
			if err != nil {
				t.Fatalf("%s: Build: %v", handler, err)
			}
			if len(out.Errors) > 0 {
				t.Fatalf("%s: Build reported errors: %v", handler, out.Errors)
			}
			binary, _ := os.ReadFile(filepath.Join(input.Out(), "bootstrap"))
			results[handler] = result{out, string(binary)}
		}
		return results
	}
	// record writes the state after a deploy: a function that was built gets
	// a new zip, one that was not keeps the key it had.
	record := func(results map[string]result, n int) string {
		functions := map[string][3]string{}
		for handler, name := range names {
			r := results[handler]
			k := r.out.Deployed
			if k == "" {
				k = key(name, n)
			}
			functions[name] = [3]string{r.out.Signature, k, zip(name)}
		}
		return writeDeployedState(t, functions)
	}
	expectBuilt := func(results map[string]result, n int, want ...string) {
		t.Helper()
		wanted := map[string]bool{}
		for _, handler := range want {
			wanted[handler] = true
		}
		for handler, r := range results {
			built := r.out.Deployed == ""
			if built != (r.binary != "") {
				t.Errorf("deploy %d: %s: Deployed=%q and binary present=%v disagree", n, handler, r.out.Deployed, r.binary != "")
			}
			if r.out.Signature == "" {
				t.Errorf("deploy %d: %s has no signature to record", n, handler)
			}
			if built && !wanted[handler] {
				t.Errorf("deploy %d: %s was built, and nothing it imports changed", n, handler)
			}
			if !built && wanted[handler] {
				t.Errorf("deploy %d: %s was not built, and what it imports changed: the change would not be deployed", n, handler)
			}
		}
	}

	// 1: nothing deployed yet, everything is built
	first := deploy("")
	expectBuilt(first, 1, signFixtureHandlers...)
	state := record(first, 1)

	// 2: nothing changed, nothing is built, every function keeps its zip
	second := deploy(state)
	expectBuilt(second, 2)
	for handler, name := range names {
		if got := second[handler].out.Deployed; got != key(name, 1) {
			t.Errorf("deploy 2: %s keeps %q, want the zip of the first deploy", handler, got)
		}
	}
	state = record(second, 2)

	// 3: a package two handlers import, through another package for one of them
	mustWriteFile(t, filepath.Join(root, "shared/z/z.go"), "package z\n\nfunc Name() string { return \"z2\" }\n")
	third := deploy(state)
	expectBuilt(third, 3, "handlers/b", "handlers/ab")
	if third["handlers/b"].binary == first["handlers/b"].binary {
		t.Error("deploy 3: the binary does not carry the change")
	}
	state = record(third, 3)

	// 4: nothing changed since, nothing is built
	fourth := deploy(state)
	expectBuilt(fourth, 4)
	state = record(fourth, 4)

	// 5: a handler's own file, and a package nobody imports
	mustWriteFile(t, filepath.Join(root, "handlers/c/main.go"), "package main\n\nfunc main() { println(\"c2\") }\n")
	mustWriteFile(t, filepath.Join(root, "shared/lonely/lonely.go"), "package lonely\n\nfunc Name() string { return \"l2\" }\n")
	fifth := deploy(state)
	expectBuilt(fifth, 5, "handlers/c")
	state = record(fifth, 5)

	// 6: the change of deploy 3 is undone. The state says the handlers run
	// the changed code, so they are built again, back to the first binary.
	mustWriteFile(t, filepath.Join(root, "shared/z/z.go"), signFixture["shared/z/z.go"])
	sixth := deploy(state)
	expectBuilt(sixth, 6, "handlers/b", "handlers/ab")
	if sixth["handlers/b"].binary != first["handlers/b"].binary {
		t.Error("deploy 6: undoing the change did not give back the first binary")
	}

	// 7: a deploy that failed half way. It uploaded the new zip of two
	// functions and never got to record their signatures: the state has the
	// old signature next to a newer code object. Then the change is undone and
	// the sources sign like the recorded signature again. Those two functions
	// run the changed code, so they must be built; the others are untouched.
	recorded := map[string][3]string{}
	for handler, name := range names {
		k := sixth[handler].out.Deployed
		if k == "" {
			k = key(name, 6)
		}
		recorded[name] = [3]string{sixth[handler].out.Signature, k, zip(name)}
	}
	built := deployWithSplitState(t, cfgPath, root, properties, names, zip, recorded, map[string]bool{"FnA": true, "FnAB": true})
	for handler, name := range names {
		want := handler == "handlers/a" || handler == "handlers/ab"
		if built[name] != want {
			t.Errorf("after a failed deploy and a revert: %s built=%v, want %v", handler, built[name], want)
		}
	}
}

// deployWithSplitState runs one deploy from a state where, for the functions
// in split, the code object is not the one the signature was recorded with.
// It returns which functions were built.
func deployWithSplitState(t *testing.T, cfgPath, root string, properties []byte, names map[string]string,
	zip func(string) string, functions map[string][3]string, split map[string]bool,
) map[string]bool {
	t.Helper()
	const stack = "urn:pulumi:prod::app::"
	resources := []map[string]any{}
	for name, f := range functions {
		recorded := f[1]
		object := recorded
		if split[name] {
			object = recorded + "-uploaded-by-the-failed-deploy"
		}
		resources = append(resources,
			map[string]any{
				"urn":     stack + "sst:aws:Function::" + name,
				"type":    "sst:aws:Function",
				"outputs": map[string]any{"_code": map[string]any{"signature": f[0], "key": recorded}},
			},
			map[string]any{
				"urn":    stack + "sst:aws:Function$aws:s3/bucketObjectv2:BucketObjectv2::" + name + "Code",
				"type":   "aws:s3/bucketObjectv2:BucketObjectv2",
				"parent": stack + "sst:aws:Function::" + name,
				"inputs": map[string]any{"key": object, "source": map[string]any{"hash": "h", "path": f[2]}},
			},
		)
	}
	checkpoint, _ := json.Marshal(map[string]any{"latest": map[string]any{"resources": resources}})
	state, _ := json.Marshal(map[string]any{"version": 3, "checkpoint": json.RawMessage(checkpoint)})
	statePath := filepath.Join(t.TempDir(), "state.json")
	mustWriteFile(t, statePath, string(state))

	collection := runtime.NewCollection(cfgPath, New())
	collection.LoadDeployed(statePath)
	built := map[string]bool{}
	for handler, name := range names {
		out, err := collection.Build(context.Background(), &runtime.BuildInput{
			CfgPath:    cfgPath,
			FunctionID: name,
			Runtime:    "go",
			Handler:    filepath.Join(root, filepath.FromSlash(handler)),
			Properties: properties,
			Code:       zip(name),
		})
		if err != nil || len(out.Errors) > 0 {
			t.Fatalf("%s: Build: %v %v", handler, err, out)
		}
		built[name] = out.Deployed == ""
	}
	return built
}

// sst dev deploys more than once in one process, and a function it builds for
// real goes through the deploy path each time. A change made between two
// deploys has to be built by the second, not signed with what the first read.
func TestDeploy_SecondDeployOfTheSameProcessSeesTheChange(t *testing.T) {
	root := newSignFixture(t)
	t.Setenv("GOFLAGS", "-mod=mod -trimpath")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")
	cfgPath := filepath.Join(t.TempDir(), "sst.config.ts")
	properties, _ := json.Marshal(Properties{Architecture: "arm64"})
	zip := filepath.Join(filepath.Dir(cfgPath), ".sst", "artifacts", "FnA", "code.zip")
	input := func() *runtime.BuildInput {
		return &runtime.BuildInput{
			CfgPath: cfgPath, FunctionID: "FnA", Runtime: "go",
			Handler: filepath.Join(root, "handlers", "a"), Properties: properties, Code: zip,
		}
	}

	// one collection, as one process has
	collection := runtime.NewCollection(cfgPath, New())
	collection.LoadDeployed(filepath.Join(t.TempDir(), "no-state-yet.json"))
	first, err := collection.Build(context.Background(), input())
	if err != nil || len(first.Errors) > 0 || first.Deployed != "" || first.Signature == "" {
		t.Fatalf("first deploy: %+v, %v", first, err)
	}
	state := writeDeployedState(t, map[string][3]string{"FnA": {first.Signature, "assets/FnA-code-1.zip", zip}})

	// second deploy, nothing changed: kept
	collection.LoadDeployed(state)
	second, err := collection.Build(context.Background(), input())
	if err != nil || second.Deployed == "" {
		t.Fatalf("second deploy built an unchanged function: %+v, %v", second, err)
	}

	// third deploy of the same process, after a change to a shared package
	mustWriteFile(t, filepath.Join(root, "shared/x/x.go"), "package x\n\nfunc Name() string { return \"x2\" }\n")
	collection.LoadDeployed(state)
	third, err := collection.Build(context.Background(), input())
	if err != nil || len(third.Errors) > 0 {
		t.Fatalf("third deploy: %+v, %v", third, err)
	}
	if third.Deployed != "" {
		t.Fatal("the change was not built: the function would keep running the old code")
	}
	if third.Signature == first.Signature {
		t.Error("the third deploy signed with what the first one read")
	}
	if third.Replaces != first.Signature || third.Reason != "differs from the deployed one" {
		t.Errorf("third deploy: replaces %q, reason %q", third.Replaces, third.Reason)
	}
}
