package aws

import (
	"testing"

	"github.com/sst/sst/v3/pkg/runtime"
)

func eagerExcept(lazy ...string) func(string) bool {
	lazyRuntimes := map[string]bool{}
	for _, name := range lazy {
		lazyRuntimes[name] = true
	}
	return func(name string) bool {
		return !lazyRuntimes[name]
	}
}

func TestEagerRebuildTargets(t *testing.T) {
	targets := map[string]*runtime.BuildInput{
		"go-a":   {FunctionID: "go-a", Runtime: "go"},
		"go-b":   {FunctionID: "go-b", Runtime: "go"},
		"python": {FunctionID: "python", Runtime: "python"},
	}

	tests := []struct {
		name        string
		functionIDs []string
		runEagerly  func(string) bool
		want        map[string]bool
	}{
		{
			name:        "keeps the eager runtimes",
			functionIDs: []string{"go-a", "go-b"},
			runEagerly:  eagerExcept("python"),
			want:        map[string]bool{"go-a": true, "go-b": true},
		},
		{
			// A lazy runtime builds its handler at invocation time, so
			// pre-building it here would compile something nobody is waiting on.
			name:        "drops the lazy runtimes",
			functionIDs: []string{"go-a", "python"},
			runEagerly:  eagerExcept("python"),
			want:        map[string]bool{"go-a": true},
		},
		{
			// Two workers of the same handler must not queue two compiles.
			name:        "collapses workers sharing a function",
			functionIDs: []string{"go-a", "go-a", "go-a"},
			runEagerly:  eagerExcept(),
			want:        map[string]bool{"go-a": true},
		},
		{
			// Without a BuildInput there is nothing to hand to Build, and the
			// old sequential path passed that nil straight through.
			name:        "skips a worker whose target is gone",
			functionIDs: []string{"go-a", "vanished"},
			runEagerly:  eagerExcept(),
			want:        map[string]bool{"go-a": true},
		},
		{
			name:        "no workers, nothing to build",
			functionIDs: nil,
			runEagerly:  eagerExcept(),
			want:        map[string]bool{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eagerRebuildTargets(tt.functionIDs, targets, tt.runEagerly)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for functionID := range tt.want {
				if !got[functionID] {
					t.Errorf("missing %q: got %v, want %v", functionID, got, tt.want)
				}
			}
		})
	}
}
