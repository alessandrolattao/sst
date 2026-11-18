package journal

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"reflect"
	"slices"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
)

// elision tells which journal entries leave the state as it was, so they do
// not make it worth saving and pushing again.
//
// Pulumi's own snapshot manager skips the checkpoint write of a `same` step
// that changed nothing (sameSnapshotMutation.mustWrite), and marks the journal
// entry as such with ElideWrite. That flag stays in the CLI: the entries it
// sends over the HTTP API do not carry it, and every one comes with its
// secrets encrypted again, under a new nonce. Without this, a deploy where most
// resources are unchanged rewrites and uploads the whole state after every
// step, for a file that only differs in its ciphertexts.
//
// So the decision is made again here, from what the entries carry: a step that
// began without a pending operation, and ended with a resource equal to the
// one it replaces, ciphertexts and the fields mustWrite ignores aside. Skipped
// entries are still applied: only the save waits for one that changes
// something, or for the end of the update.
type elision struct {
	// base is the deployment the update starts from: RemoveOld indexes it.
	base *apitype.DeploymentV3
	// replaced holds what an entry put in place of a base resource.
	replaced map[int64]*apitype.ResourceV3
	// produced holds the resource each operation of this update left.
	produced map[int64]*apitype.ResourceV3
	// quiet holds the operations that began without a pending operation.
	quiet map[int64]bool
	// off stops skipping once the base changed under the indices.
	off bool
}

func newElision(base *apitype.DeploymentV3) *elision {
	return &elision{
		base:     base,
		replaced: map[int64]*apitype.ResourceV3{},
		produced: map[int64]*apitype.ResourceV3{},
		quiet:    map[int64]bool{},
	}
}

// unchanged reports whether applying the entry leaves the state as it is. It
// is asked before the entry is applied, and record after.
func (e *elision) unchanged(entry apitype.JournalEntry) bool {
	if e.off {
		return false
	}
	switch entry.Kind {
	case apitype.JournalEntryKindBegin:
		// Only a pending operation shows up in the state.
		return entry.Operation == nil
	case apitype.JournalEntryKindSuccess:
		if !e.quiet[entry.OperationID] || entry.State == nil || entry.IsRefresh ||
			entry.DeleteOld != nil || entry.DeleteNew != nil ||
			entry.PendingReplacementOld != nil || entry.PendingReplacementNew != nil {
			return false
		}
		old := e.previous(entry)
		return old != nil && sameResource(old, entry.State)
	case apitype.JournalEntryKindOutputs:
		if entry.State == nil {
			return false
		}
		old := e.previous(entry)
		return old != nil && sameResource(old, entry.State)
	case apitype.JournalEntryKindSecretsManager:
		// The replayer keeps the base's own when it is the same one.
		current, next := e.base.SecretsProviders, entry.SecretsProvider
		return current != nil && next != nil && current.Type == next.Type && bytes.Equal(current.State, next.State)
	}
	return false
}

// record follows what an applied entry did to the resources.
func (e *elision) record(entry apitype.JournalEntry) {
	switch entry.Kind {
	case apitype.JournalEntryKindBegin:
		e.quiet[entry.OperationID] = entry.Operation == nil
	case apitype.JournalEntryKindSuccess:
		delete(e.quiet, entry.OperationID)
		if entry.State != nil {
			e.produced[entry.OperationID] = entry.State
		}
	case apitype.JournalEntryKindFailure:
		delete(e.quiet, entry.OperationID)
	case apitype.JournalEntryKindOutputs, apitype.JournalEntryKindRefreshSuccess:
		if entry.Kind == apitype.JournalEntryKindRefreshSuccess {
			delete(e.quiet, entry.OperationID)
		}
		if entry.RemoveOld != nil {
			e.replaced[*entry.RemoveOld] = entry.State
		}
		if entry.RemoveNew != nil {
			e.produced[*entry.RemoveNew] = entry.State
		}
	case apitype.JournalEntryKindWrite, apitype.JournalEntryKindRebuiltBaseState,
		apitype.JournalEntryKindStateMigration:
		// These move the base the indices point into.
		e.off = true
	}
}

// previous is the resource an entry replaces, as the state holds it now.
func (e *elision) previous(entry apitype.JournalEntry) *apitype.ResourceV3 {
	switch {
	case entry.RemoveOld != nil && entry.RemoveNew == nil:
		if state, ok := e.replaced[*entry.RemoveOld]; ok {
			return state
		}
		if i := *entry.RemoveOld; i >= 0 && i < int64(len(e.base.Resources)) {
			return &e.base.Resources[i]
		}
	case entry.RemoveNew != nil && entry.RemoveOld == nil:
		return e.produced[*entry.RemoveNew]
	}
	return nil
}

// ignoredFields are what mustWrite leaves out too: advisory, or metadata.
var ignoredFields = []string{"initErrors", "sourcePosition", "stackTrace", "created", "modified"}

// sameResource compares two resource states as mustWrite does, except that
// any two secrets count as equal: their ciphertexts differ on every
// encryption, and the plaintext is not at hand. Inputs and outputs are the only
// places secrets live, and a `same` step keeps its old outputs, so the only
// change this misses is a secret input the provider ignored. It is written by
// the next save that is not skipped, at the latest when the update ends.
func sameResource(a, b *apitype.ResourceV3) bool {
	na, okA := normalized(a)
	nb, okB := normalized(b)
	return okA && okB && reflect.DeepEqual(na, nb)
}

func normalized(r *apitype.ResourceV3) (map[string]any, bool) {
	data, err := jsonv2.Marshal(r)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := jsonv2.Unmarshal(data, &m); err != nil {
		return nil, false
	}
	for _, field := range ignoredFields {
		delete(m, field)
	}
	if deps, ok := m["dependencies"].([]any); ok {
		slices.SortFunc(deps, func(x, y any) int {
			xs, _ := x.(string)
			ys, _ := y.(string)
			switch {
			case xs < ys:
				return -1
			case xs > ys:
				return 1
			}
			return 0
		})
	}
	for _, field := range []string{"inputs", "outputs"} {
		if v, ok := m[field]; ok {
			m[field] = withoutCiphertexts(v)
		}
	}
	return m, true
}

// withoutCiphertexts replaces every secret with the same marker.
func withoutCiphertexts(v any) any {
	switch v := v.(type) {
	case map[string]any:
		if v[resource.SigKey] == resource.SecretSig {
			return "secret"
		}
		for k, item := range v {
			v[k] = withoutCiphertexts(item)
		}
		return v
	case []any:
		for i, item := range v {
			v[i] = withoutCiphertexts(item)
		}
		return v
	}
	return v
}
