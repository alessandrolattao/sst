package runtime_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sst/sst/v3/pkg/bus"
	"github.com/sst/sst/v3/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockRuntime struct {
	matchFn func(string) bool
	buildFn func(*runtime.BuildInput) (*runtime.BuildOutput, error)
}

func (m *mockRuntime) Match(r string) bool {
	return m.matchFn(r)
}
func (m *mockRuntime) Build(ctx context.Context, input *runtime.BuildInput) (*runtime.BuildOutput, error) {
	if m.buildFn == nil {
		return nil, nil
	}
	return m.buildFn(input)
}
func (m *mockRuntime) Run(ctx context.Context, input *runtime.RunInput) (runtime.Worker, error) {
	return nil, nil
}
func (m *mockRuntime) ShouldRebuild(functionID string, path string) bool {
	return false
}
func (m *mockRuntime) ShouldRunEagerly() bool {
	return true
}

func TestBuildInputOut(t *testing.T) {
	cfgPath := filepath.Join("/project", "sst.config.ts")
	workingDir := filepath.Join("/project", ".sst")

	t.Run("dev mode", func(t *testing.T) {
		input := &runtime.BuildInput{
			CfgPath:    cfgPath,
			Dev:        true,
			FunctionID: "myFunc",
		}
		expected := filepath.Join(workingDir, "artifacts", "myFunc-dev")
		assert.Equal(t, expected, input.Out())
	})

	t.Run("prod mode", func(t *testing.T) {
		input := &runtime.BuildInput{
			CfgPath:    cfgPath,
			Dev:        false,
			FunctionID: "myFunc",
		}
		expected := filepath.Join(workingDir, "artifacts", "myFunc-src")
		assert.Equal(t, expected, input.Out())
	})
}

func TestCollectionRuntime(t *testing.T) {
	t.Run("matching runtime found", func(t *testing.T) {
		mr := &mockRuntime{matchFn: func(r string) bool { return r == "nodejs" }}
		c := runtime.NewCollection("cfg", mr)

		rt, ok := c.Runtime("nodejs")
		require.True(t, ok)
		assert.Equal(t, mr, rt)
	})

	t.Run("no match", func(t *testing.T) {
		mr := &mockRuntime{matchFn: func(r string) bool { return false }}
		c := runtime.NewCollection("cfg", mr)

		_, ok := c.Runtime("python")
		assert.False(t, ok)
	})

	t.Run("empty collection", func(t *testing.T) {
		c := runtime.NewCollection("cfg")

		_, ok := c.Runtime("anything")
		assert.False(t, ok)
	})
}

func TestCollectionBuildPublishesCompletion(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "sst.config.ts")

	t.Run("compiled handler", func(t *testing.T) {
		mr := &mockRuntime{
			matchFn: func(r string) bool { return r == "go" },
			buildFn: func(input *runtime.BuildInput) (*runtime.BuildOutput, error) {
				return &runtime.BuildOutput{Handler: input.Handler}, nil
			},
		}
		c := runtime.NewCollection(cfgPath, mr)
		events := bus.Subscribe(&runtime.BuildCompleteEvent{})

		_, err := c.Build(context.Background(), &runtime.BuildInput{
			CfgPath:    cfgPath,
			FunctionID: "myFunc",
			Handler:    "./services/api/handler.go",
			Runtime:    "go",
		})
		require.NoError(t, err)

		select {
		case evt := <-events:
			complete, ok := evt.(*runtime.BuildCompleteEvent)
			require.True(t, ok)
			assert.Equal(t, "myFunc", complete.FunctionID)
			assert.Equal(t, "./services/api/handler.go", complete.Handler)
			assert.Equal(t, "go", complete.Runtime)
			assert.Positive(t, complete.Duration)
		case <-time.After(time.Second):
			t.Fatal("no build complete event published")
		}
	})

	t.Run("prebuilt bundle", func(t *testing.T) {
		mr := &mockRuntime{matchFn: func(r string) bool { return r == "go" }}
		c := runtime.NewCollection(cfgPath, mr)
		events := bus.Subscribe(&runtime.BuildCompleteEvent{})

		_, err := c.Build(context.Background(), &runtime.BuildInput{
			CfgPath:    cfgPath,
			FunctionID: "myFunc",
			Bundle:     t.TempDir(),
			Runtime:    "go",
		})
		require.NoError(t, err)

		select {
		case evt := <-events:
			t.Fatalf("unexpected event for a bundle that was never compiled: %v", evt)
		case <-time.After(50 * time.Millisecond):
		}
	})
}
