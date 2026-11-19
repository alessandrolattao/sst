package ui

import (
	"context"
	"testing"

	"github.com/sst/sst/v3/pkg/project"
	"github.com/sst/sst/v3/pkg/runtime"
)

const (
	testSignature = "02d3ad858d6526369ab0de3c9c624a5a144b9072e3779de561cbcef5bc03637c"
	testReplaced  = "9f41c0aa51ec783730b49825c0dc01ac30f3df271c8fb4b6ecc454c5ce7d1cff"
)

func TestSignatureNote(t *testing.T) {
	cases := []struct {
		name string
		evt  runtime.BuildEvent
		want string
	}{
		{
			name: "a build that replaces another shows both signatures",
			evt:  runtime.BuildEvent{Signature: testSignature, Replaces: testReplaced, Reason: "differs from the deployed one"},
			want: " signature 9f41c0aa → 02d3ad85",
		},
		{
			name: "a first build says there was nothing to keep",
			evt:  runtime.BuildEvent{Signature: testSignature, Reason: "nothing deployed with a signature yet"},
			want: " signature 02d3ad85, nothing deployed with a signature yet",
		},
		{
			name: "a build that signs like the deployed one says why it ran",
			evt:  runtime.BuildEvent{Signature: testSignature, Replaces: testSignature, Reason: "equals the deployed one, but its zip is not the one the signature was recorded with"},
			want: " signature 02d3ad85, equals the deployed one, but its zip is not the one the signature was recorded with",
		},
		{
			name: "a signed build with no reason shows its signature",
			evt:  runtime.BuildEvent{Signature: testSignature},
			want: " signature 02d3ad85",
		},
		{
			name: "an unsigned build says nothing",
			evt:  runtime.BuildEvent{Reason: "not signed: the runtime does not sign its builds"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := signatureNote(&tc.evt); got != tc.want {
				t.Errorf("signatureNote = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestShortSignature(t *testing.T) {
	if got := shortSignature(testSignature); got != "02d3ad85" {
		t.Errorf("shortSignature = %q", got)
	}
	if got := shortSignature("abc"); got != "abc" {
		t.Errorf("a short signature was cut: %q", got)
	}
	if got := shortSignature(""); got != "" {
		t.Errorf("no signature gave %q", got)
	}
}

func TestUnchangedSummary(t *testing.T) {
	if got, want := unchangedSummary(1), "1 function, its signature matches the deployed one"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := unchangedSummary(1297), "1297 functions, signatures match the deployed ones"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The functions a deploy did not build are counted, said once when the deploy
// ends, and the next deploy starts from zero.
func TestUnchangedFunctionsAreCountedPerDeploy(t *testing.T) {
	u := New(context.Background(), WithSilent)
	for range 3 {
		u.Event(&runtime.UnchangedEvent{FunctionID: "Fn", Handler: "services/fn", Signature: testSignature})
	}
	u.Event(&runtime.BuildEvent{FunctionID: "Other", Handler: "services/other", Signature: testSignature})
	if u.unchanged != 3 {
		t.Fatalf("counted %d unchanged functions, want 3", u.unchanged)
	}

	// what an earlier deploy left on screen is not this deploy's summary
	u.Event(&project.CompleteEvent{Old: true})
	if u.unchanged != 3 {
		t.Fatalf("an old completion reset the count to %d", u.unchanged)
	}

	u.Event(&project.CompleteEvent{Finished: true})
	if u.unchanged != 0 {
		t.Fatalf("the count is %d after the deploy ended, want 0", u.unchanged)
	}

	u.Event(&runtime.UnchangedEvent{FunctionID: "Fn", Handler: "services/fn", Signature: testSignature})
	if u.unchanged != 1 {
		t.Fatalf("the next deploy counted %d, want 1", u.unchanged)
	}
}
