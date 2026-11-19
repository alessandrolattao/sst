package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sst/sst/v3/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signingRuntime signs a build with whatever the test tells it to, and counts
// the builds it is asked to run.
type signingRuntime struct {
	mockRuntime
	signature string
	signErr   error
	builds    int
}

func (m *signingRuntime) Build(ctx context.Context, input *runtime.BuildInput) (*runtime.BuildOutput, error) {
	m.builds++
	if err := os.WriteFile(filepath.Join(input.Out(), "bootstrap"), []byte("binary"), 0o755); err != nil {
		return nil, err
	}
	return &runtime.BuildOutput{Handler: "bootstrap"}, nil
}

func (m *signingRuntime) Sign(ctx context.Context, input *runtime.BuildInput) (string, string, error) {
	return m.signature, "bootstrap", m.signErr
}

func newSigning(signature string) *signingRuntime {
	return &signingRuntime{
		mockRuntime: mockRuntime{matchFn: func(r string) bool { return r == "go" }},
		signature:   signature,
	}
}

const (
	testStack   = "urn:pulumi:prod::app::"
	testCodeKey = "assets/Fn-code-aaaa.zip"
)

type stateResource map[string]any

func functionResource(name string, code map[string]any) stateResource {
	outputs := map[string]any{"_metadata": map[string]any{"handler": "x"}}
	if code != nil {
		outputs["_code"] = code
	}
	return stateResource{
		"urn":     testStack + "sst:aws:Function::" + name,
		"type":    "sst:aws:Function",
		"outputs": outputs,
	}
}

func codeResource(name, key, path string) stateResource {
	return stateResource{
		"urn":    testStack + "sst:aws:Function$aws:s3/bucketObjectv2:BucketObjectv2::" + name + "Code",
		"type":   "aws:s3/bucketObjectv2:BucketObjectv2",
		"parent": testStack + "sst:aws:Function::" + name,
		"inputs": map[string]any{
			"key":    key,
			"source": map[string]any{"hash": "aaaa", "path": path},
		},
	}
}

func writeState(t *testing.T, resources ...stateResource) string {
	t.Helper()
	checkpoint, err := json.Marshal(map[string]any{
		"stack":  "organization/app/prod",
		"latest": map[string]any{"resources": resources},
	})
	require.NoError(t, err)
	state, err := json.Marshal(map[string]any{"version": 3, "checkpoint": json.RawMessage(checkpoint)})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, os.WriteFile(path, state, 0o644))
	return path
}

type deployedFixture struct {
	t       *testing.T
	cfgPath string
	code    string
}

func newDeployedFixture(t *testing.T) *deployedFixture {
	dir := t.TempDir()
	return &deployedFixture{t: t, cfgPath: filepath.Join(dir, "sst.config.ts"), code: filepath.Join(dir, "code.zip")}
}

func (f *deployedFixture) input() *runtime.BuildInput {
	return &runtime.BuildInput{
		CfgPath:       f.cfgPath,
		FunctionID:    "Fn",
		Runtime:       "go",
		Handler:       "services/fn",
		Properties:    json.RawMessage(`{"architecture":"arm64"}`),
		Links:         map[string]json.RawMessage{"Bucket": json.RawMessage(`{"name":"b"}`)},
		EncryptionKey: "a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2U=",
		Code:          f.code,
	}
}

// signatureOf builds once with nothing deployed and returns the signature the
// collection gave the build, the one a deploy would record in the state.
func (f *deployedFixture) signatureOf(input *runtime.BuildInput, rt *signingRuntime) string {
	f.t.Helper()
	// built somewhere else, so it leaves nothing where the test looks
	elsewhere := *input
	elsewhere.CfgPath = filepath.Join(f.t.TempDir(), "sst.config.ts")
	out, err := runtime.NewCollection(elsewhere.CfgPath, newSigning(rt.signature)).Build(context.Background(), &elsewhere)
	require.NoError(f.t, err)
	require.NotEmpty(f.t, out.Signature)
	return out.Signature
}

func TestBuildKeepsDeployedCodeWhenTheSignatureMatches(t *testing.T) {
	f := newDeployedFixture(t)
	rt := newSigning("go-signature")
	signature := f.signatureOf(f.input(), rt)

	collection := runtime.NewCollection(f.cfgPath, rt)
	collection.LoadDeployed(writeState(t,
		functionResource("Fn", map[string]any{"signature": signature, "key": testCodeKey}),
		codeResource("Fn", testCodeKey, f.code),
	))

	input := f.input()
	// what an earlier build left behind stays as it is
	require.NoError(t, os.MkdirAll(input.Out(), 0o755))
	left := filepath.Join(input.Out(), "left")
	require.NoError(t, os.WriteFile(left, []byte("x"), 0o644))

	out, err := collection.Build(context.Background(), input)
	require.NoError(t, err)
	assert.Equal(t, testCodeKey, out.Deployed)
	assert.Equal(t, signature, out.Signature)
	assert.Equal(t, "bootstrap", out.Handler)
	assert.Empty(t, out.Errors)
	assert.NotNil(t, out.Sourcemaps)
	assert.Equal(t, 0, rt.builds, "the runtime was asked to build")
	assert.FileExists(t, left)
	assert.NoFileExists(t, filepath.Join(input.Out(), "resource.enc"))
}

func TestBuildRunsWheneverTheDeployedCodeIsNotCertain(t *testing.T) {
	type setup struct {
		f     *deployedFixture
		rt    *signingRuntime
		input *runtime.BuildInput
		// resources of the state, nil for no state at all
		state []stateResource
	}
	cases := []struct {
		name string
		// edit changes a setup where the build would be skipped
		edit func(s *setup, signature string)
		// the build still reports a signature, to be recorded for next time
		signed bool
	}{
		{
			name:   "nothing loaded from the state",
			edit:   func(s *setup, signature string) { s.state = nil },
			signed: true,
		},
		{
			name:   "an empty state",
			edit:   func(s *setup, signature string) { s.state = []stateResource{} },
			signed: true,
		},
		{
			name:   "the runtime signs the build differently",
			edit:   func(s *setup, signature string) { s.rt.signature = "another-go-signature" },
			signed: true,
		},
		{
			name: "the linked resources changed",
			edit: func(s *setup, signature string) {
				s.input.Links = map[string]json.RawMessage{"Bucket": json.RawMessage(`{"name":"other"}`)}
			},
			signed: true,
		},
		{
			name: "a resource was linked",
			edit: func(s *setup, signature string) {
				s.input.Links["Queue"] = json.RawMessage(`{"url":"q"}`)
			},
			signed: true,
		},
		{
			name: "the encryption key changed",
			edit: func(s *setup, signature string) {
				s.input.EncryptionKey = "b3RoZXJvdGhlcm90aGVyb3RoZXJvdGhlcm90aGVyb3Q="
			},
			signed: true,
		},
		{
			name:   "the architecture changed",
			edit:   func(s *setup, signature string) { s.input.Properties = json.RawMessage(`{"architecture":"x86_64"}`) },
			signed: true,
		},
		{
			name: "the code object is not the one the signature was recorded for",
			edit: func(s *setup, signature string) {
				s.state[1] = codeResource("Fn", "assets/Fn-code-bbbb.zip", s.f.code)
			},
			signed: true,
		},
		{
			name: "the zip was read from another path",
			edit: func(s *setup, signature string) {
				s.state[1] = codeResource("Fn", testCodeKey, filepath.Join(s.f.t.TempDir(), "code.zip"))
			},
			signed: true,
		},
		{
			name: "the code object has no source path",
			edit: func(s *setup, signature string) {
				s.state[1]["inputs"] = map[string]any{"key": testCodeKey}
			},
			signed: true,
		},
		{
			name:   "there is no code object",
			edit:   func(s *setup, signature string) { s.state = s.state[:1] },
			signed: true,
		},
		{
			name: "the code object belongs to another resource",
			edit: func(s *setup, signature string) {
				s.state[1]["parent"] = testStack + "sst:aws:Function::Other"
			},
			signed: true,
		},
		{
			name: "the code object is being deleted",
			edit: func(s *setup, signature string) {
				s.state[1]["delete"] = true
			},
			signed: true,
		},
		{
			name: "the function is being deleted",
			edit: func(s *setup, signature string) {
				s.state[0]["delete"] = true
			},
			signed: true,
		},
		{
			name: "the function recorded no signature",
			edit: func(s *setup, signature string) {
				s.state[0] = functionResource("Fn", nil)
			},
			signed: true,
		},
		{
			name: "the function recorded a signature and no key",
			edit: func(s *setup, signature string) {
				s.state[0] = functionResource("Fn", map[string]any{"signature": signature})
			},
			signed: true,
		},
		{
			name: "the recorded signature is not a string",
			edit: func(s *setup, signature string) {
				s.state[0] = functionResource("Fn", map[string]any{"signature": 12, "key": testCodeKey})
			},
			signed: true,
		},
		{
			name: "two functions have the name",
			edit: func(s *setup, signature string) {
				twin := functionResource("Fn", map[string]any{"signature": signature, "key": testCodeKey})
				twin["urn"] = testStack + "sst:aws:Cron$sst:aws:Function::Fn"
				s.state = append(s.state, twin)
			},
			signed: true,
		},
		{
			name: "the signature belongs to another function",
			edit: func(s *setup, signature string) {
				s.state = []stateResource{
					functionResource("Other", map[string]any{"signature": signature, "key": testCodeKey}),
					codeResource("Other", testCodeKey, s.f.code),
				}
			},
			signed: true,
		},
		{
			name: "files are copied into the zip",
			edit: func(s *setup, signature string) {
				from := filepath.Join(s.f.t.TempDir(), "extra.txt")
				require.NoError(s.f.t, os.WriteFile(from, []byte("x"), 0o644))
				s.input.CopyFiles = []struct {
					From string `json:"from"`
					To   string `json:"to"`
				}{{From: from, To: "extra.txt"}}
			},
		},
		{
			name: "the caller cannot keep the deployed zip",
			edit: func(s *setup, signature string) { s.input.Code = "" },
		},
		{
			name: "a dev build",
			edit: func(s *setup, signature string) { s.input.Dev = true },
		},
		{
			name: "the runtime cannot sign this build",
			edit: func(s *setup, signature string) { s.rt.signature = "" },
		},
		{
			name: "the runtime fails to sign",
			edit: func(s *setup, signature string) { s.rt.signErr = errors.New("go list failed") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeployedFixture(t)
			rt := newSigning("go-signature")
			signature := f.signatureOf(f.input(), rt)
			s := &setup{
				f: f, rt: rt, input: f.input(),
				state: []stateResource{
					functionResource("Fn", map[string]any{"signature": signature, "key": testCodeKey}),
					codeResource("Fn", testCodeKey, f.code),
				},
			}
			tc.edit(s, signature)

			collection := runtime.NewCollection(f.cfgPath, rt)
			if s.state != nil {
				collection.LoadDeployed(writeState(t, s.state...))
			}
			out, err := collection.Build(context.Background(), s.input)
			require.NoError(t, err)
			assert.Empty(t, out.Deployed, "the build was skipped")
			assert.Equal(t, 1, rt.builds)
			assert.FileExists(t, filepath.Join(s.input.Out(), "bootstrap"))
			if tc.signed {
				assert.NotEmpty(t, out.Signature)
			} else {
				assert.Empty(t, out.Signature)
			}
		})
	}
}

func TestBuildRunsWhenTheStateCannotBeRead(t *testing.T) {
	for name, contents := range map[string]string{
		"missing":      "",
		"not json":     "{",
		"no latest":    `{"version":3,"checkpoint":{"stack":"s"}}`,
		"wrong shapes": `{"version":3,"checkpoint":{"latest":{"resources":[{"urn":"urn:pulumi:prod::app::sst:aws:Function::Fn","type":"sst:aws:Function","outputs":{"_code":"text"}}]}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newDeployedFixture(t)
			rt := newSigning("go-signature")
			path := filepath.Join(t.TempDir(), "state.json")
			if contents != "" {
				require.NoError(t, os.WriteFile(path, []byte(contents), 0o644))
			}
			collection := runtime.NewCollection(f.cfgPath, rt)
			collection.LoadDeployed(path)
			out, err := collection.Build(context.Background(), f.input())
			require.NoError(t, err)
			assert.Empty(t, out.Deployed)
			assert.Equal(t, 1, rt.builds)
		})
	}
}

func TestBuildWithoutASignerIsUntouched(t *testing.T) {
	f := newDeployedFixture(t)
	rt := &outputRuntime{mockRuntime{matchFn: func(r string) bool { return r == "go" }}}
	collection := runtime.NewCollection(f.cfgPath, rt)
	collection.LoadDeployed(writeState(t,
		functionResource("Fn", map[string]any{"signature": "anything", "key": testCodeKey}),
		codeResource("Fn", testCodeKey, f.code),
	))
	out, err := collection.Build(context.Background(), f.input())
	require.NoError(t, err)
	assert.Empty(t, out.Deployed)
	assert.Empty(t, out.Signature)
}

// A second deploy in the same process starts from the state the first one
// left, not from what was loaded before it.
func TestLoadDeployedReplacesWhatWasLoaded(t *testing.T) {
	f := newDeployedFixture(t)
	rt := newSigning("go-signature")
	signature := f.signatureOf(f.input(), rt)
	matching := writeState(t,
		functionResource("Fn", map[string]any{"signature": signature, "key": testCodeKey}),
		codeResource("Fn", testCodeKey, f.code),
	)

	collection := runtime.NewCollection(f.cfgPath, rt)
	collection.LoadDeployed(matching)
	out, err := collection.Build(context.Background(), f.input())
	require.NoError(t, err)
	require.Equal(t, testCodeKey, out.Deployed)

	collection.LoadDeployed(writeState(t))
	out, err = collection.Build(context.Background(), f.input())
	require.NoError(t, err)
	assert.Empty(t, out.Deployed)

	collection.LoadDeployed(filepath.Join(t.TempDir(), "gone.json"))
	out, err = collection.Build(context.Background(), f.input())
	require.NoError(t, err)
	assert.Empty(t, out.Deployed)

	collection.LoadDeployed(matching)
	out, err = collection.Build(context.Background(), f.input())
	require.NoError(t, err)
	assert.Equal(t, testCodeKey, out.Deployed)
}

// Many functions are built at once while the state is already loaded.
func TestBuildKeepsDeployedCodeConcurrently(t *testing.T) {
	f := newDeployedFixture(t)
	rt := newSigning("go-signature")
	signature := f.signatureOf(f.input(), rt)
	collection := runtime.NewCollection(f.cfgPath, rt)
	collection.LoadDeployed(writeState(t,
		functionResource("Fn", map[string]any{"signature": signature, "key": testCodeKey}),
		codeResource("Fn", testCodeKey, f.code),
	))
	done := make(chan string, 32)
	for range 32 {
		go func() {
			out, err := collection.Build(context.Background(), f.input())
			if err != nil {
				done <- err.Error()
				return
			}
			done <- out.Deployed
		}()
	}
	for range 32 {
		assert.Equal(t, testCodeKey, <-done)
	}
}
