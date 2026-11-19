package golang

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/sst/sst/v3/internal/fs"
	"github.com/sst/sst/v3/pkg/process"
	"github.com/sst/sst/v3/pkg/runtime"
)

// A deploy build is signed without being run. `go list -export` compiles every
// package, which the build cache answers for the ones that did not change, and
// reports the build ID of each: the hash of what the compiler produced. A
// binary is those packages linked with fixed flags by a fixed toolchain, so the
// build IDs of everything a handler imports, with the flags, the toolchain and
// the module files, sign the binary. One `go list` over the module signs every
// handler in it, in about the time one build takes.
//
// The signature errs on the side of building. It changes whenever a package
// the handler imports compiles to something else, even when the linker would
// then drop the difference; it never stays the same across a change.

const (
	// what Build passes to a deploy build, kept here so the two cannot drift
	deployLdflags  = "-s -w -buildid="
	deployBuildvcs = "-buildvcs=false"
)

// the go env values that change what a build produces
var signedEnv = []string{
	"GOVERSION", "GOOS", "GOARCH", "CGO_ENABLED", "GOFLAGS", "GOEXPERIMENT", "GOFIPS140", "GOWORK",
	"GOARM64", "GOAMD64", "GOARM", "GO386", "GOMIPS", "GOMIPS64", "GOPPC64", "GORISCV64", "GOWASM",
}

func deployEnv(env []string, architecture string) []string {
	return append(env,
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH="+goarchFromArchitecture(architecture),
	)
}

// signedModule holds the signature of every handler of one module, for one
// target architecture, keyed by the handler's directory.
type signedModule struct {
	once  sync.Once
	byDir map[string]string
	err   error
}

// Sign implements [runtime.Signer] for deploy builds.
func (r *Runtime) Sign(ctx context.Context, input *runtime.BuildInput) (string, string, error) {
	if input.Dev {
		return "", "", nil
	}
	var properties Properties
	json.Unmarshal(input.Properties, &properties)

	gomod, err := fs.FindUp(input.Handler, "go.mod")
	if err != nil {
		return "", "", err
	}
	root, err := realPath(filepath.Dir(gomod))
	if err != nil {
		return "", "", err
	}
	dir, err := realPath(input.Handler)
	if err != nil {
		return "", "", err
	}

	goarch := goarchFromArchitecture(properties.Architecture)
	key := root + "\x00" + goarch
	r.signedMut.Lock()
	if r.signed == nil {
		r.signed = map[string]*signedModule{}
	}
	module, ok := r.signed[key]
	if !ok {
		module = &signedModule{}
		r.signed[key] = module
	}
	r.signedMut.Unlock()

	module.once.Do(func() {
		module.byDir, module.err = signModule(ctx, root, deployEnv(os.Environ(), properties.Architecture))
	})
	if module.err != nil {
		return "", "", module.err
	}
	return module.byDir[dir], "bootstrap", nil
}

func realPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// signModule signs every main package of the module at root.
func signModule(ctx context.Context, root string, env []string) (map[string]string, error) {
	base, err := signBase(ctx, root, env)
	if err != nil {
		return nil, err
	}

	// -e: a package that does not compile takes away the signature of the
	// handlers that import it, and their build then reports the error.
	cmd := process.CommandContext(ctx, "go", "list", "-e", "-deps", "-export",
		"-json=ImportPath,Dir,Name,Deps,BuildID,Incomplete,Error,DepsErrors", "./...")
	cmd.Dir = root
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	packages, parseErr := parseSignedPackages(stdout)
	if parseErr != nil {
		process.Kill(cmd.Process)
	}
	waitErr := cmd.Wait()
	if parseErr != nil {
		return nil, parseErr
	}
	if waitErr != nil {
		return nil, waitErr
	}
	return signPackages(base, packages), nil
}

// signBase hashes what every build of the module shares: the toolchain, the
// target, the flags and the module files the binary's build info comes from.
func signBase(ctx context.Context, root string, env []string) ([]byte, error) {
	cmd := process.CommandContext(ctx, "go", append([]string{"env", "-json"}, signedEnv...)...)
	cmd.Dir = root
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var values map[string]string
	if err := json.Unmarshal(out, &values); err != nil {
		return nil, err
	}

	hash := sha256.New()
	for _, name := range signedEnv {
		writePart(hash, []byte(values[name]))
	}
	writePart(hash, []byte(deployLdflags))
	writePart(hash, []byte(deployBuildvcs))
	files := []string{filepath.Join(root, "go.mod"), filepath.Join(root, "go.sum")}
	if work := values["GOWORK"]; work != "" && work != "off" {
		files = append(files, work, work+".sum")
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		writePart(hash, data)
	}
	return hash.Sum(nil), nil
}

type signedPackage struct {
	ImportPath string   `json:"ImportPath"`
	Dir        string   `json:"Dir"`
	Name       string   `json:"Name"`
	Deps       []string `json:"Deps"`
	BuildID    string   `json:"BuildID"`
	Incomplete bool     `json:"Incomplete"`
	Error      any      `json:"Error"`
	DepsErrors []any    `json:"DepsErrors"`
}

func parseSignedPackages(r io.Reader) ([]signedPackage, error) {
	var packages []signedPackage
	dec := json.NewDecoder(r)
	for {
		var p signedPackage
		if err := dec.Decode(&p); err == io.EOF {
			return packages, nil
		} else if err != nil {
			return nil, err
		}
		packages = append(packages, p)
	}
}

// signPackages returns the signature of each main package that compiled with
// everything it imports, keyed by its directory.
func signPackages(base []byte, packages []signedPackage) map[string]string {
	ids := make(map[string]string, len(packages))
	for _, p := range packages {
		ids[p.ImportPath] = p.BuildID
	}
	// the one package the compiler never builds
	ids["unsafe"] = "unsafe"

	result := map[string]string{}
	for _, p := range packages {
		if p.Name != "main" || p.Incomplete || p.Error != nil || len(p.DepsErrors) > 0 {
			continue
		}
		paths := append([]string{p.ImportPath}, p.Deps...)
		sort.Strings(paths)
		hash := sha256.New()
		writePart(hash, base)
		signed := true
		for _, path := range paths {
			id := ids[path]
			if id == "" {
				signed = false
				break
			}
			writePart(hash, []byte(path))
			writePart(hash, []byte(id))
		}
		if !signed {
			continue
		}
		dir, err := realPath(p.Dir)
		if err != nil {
			continue
		}
		result[dir] = hex.EncodeToString(hash.Sum(nil))
	}
	return result
}

// writePart writes a length-prefixed part, so two lists of parts hash alike
// only when they are the same list.
func writePart(w io.Writer, part []byte) {
	n := len(part)
	w.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	w.Write(part)
}
