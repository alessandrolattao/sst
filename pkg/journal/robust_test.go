package journal

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
)

func bucketIDs(t *testing.T, path string) []resource.ID {
	t.Helper()
	var ids []resource.ID
	for _, r := range readLatest(t, path).Latest.Resources {
		if r.Type == bucket(0).Type {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// readOnly makes the state's folder read-only for the duration of a call, as
// a full disk or a lost permission would.
func readOnly(t *testing.T, statePath string, call func()) {
	t.Helper()
	dir := filepath.Dir(statePath)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	call()
}

// A save that cannot be written is tried again: the entries Pulumi was told
// are safe reach the file once it can be written, and a completion that
// failed on it can be retried.
func TestAFailedSaveIsTriedAgain(t *testing.T) {
	s, statePath := startOn(t)
	path, lease := begin(t, s, "update")
	code, body := call(t, s, "PATCH", path+"/journalentries", lease, creation(bucket(0)), false)
	expect(t, "the first bucket", code, http.StatusNoContent, body)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	second := bucket(1)
	more := apitype.JournalEntries{Entries: []apitype.JournalEntry{
		{Version: 2, Kind: apitype.JournalEntryKindBegin, SequenceID: 4, OperationID: 2,
			Operation: &apitype.OperationV2{Resource: second, Type: apitype.OperationTypeCreating}},
		{Version: 2, Kind: apitype.JournalEntryKindSuccess, SequenceID: 5, OperationID: 2, State: &second},
	}}
	code, body = call(t, s, "PATCH", path+"/journalentries", lease, more, false)
	expect(t, "the second bucket", code, http.StatusNoContent, body)

	readOnly(t, statePath, func() {
		if err := s.Save(); err == nil {
			t.Fatal("a save into a read-only folder succeeded")
		}
		code, body = call(t, s, "POST", path+"/complete", lease, map[string]any{"status": "succeeded"}, false)
		expect(t, "completing while the state cannot be written", code, http.StatusInternalServerError, body)
	})
	if got := bucketIDs(t, statePath); len(got) != 1 {
		t.Fatalf("buckets in the file before the retry: %v, want the first only", got)
	}

	// The CLI retries the completion.
	code, body = call(t, s, "POST", path+"/complete", lease, map[string]any{"status": "succeeded"}, false)
	expect(t, "completing again", code, http.StatusNoContent, body)
	if got := bucketIDs(t, statePath); !slices.Equal(got, []resource.ID{bucket(0).ID, bucket(1).ID}) {
		t.Errorf("buckets in the file: %v, want both", got)
	}
	s.Close()
	if err := s.Err(); err != nil {
		t.Errorf("Err after a save that went through: %v", err)
	}
}

// A final save that cannot be written is reported, so SST does not take the
// file for the update's result.
func TestAFailedFinalSaveIsReported(t *testing.T) {
	s, statePath := startOn(t)
	path, lease := begin(t, s, "update")
	code, body := call(t, s, "PATCH", path+"/journalentries", lease, creation(bucket(0)), false)
	expect(t, "a bucket", code, http.StatusNoContent, body)
	readOnly(t, statePath, s.Close)
	if s.Err() == nil {
		t.Error("Err is nil after a final save that could not be written")
	}
}

// An entry that makes Pulumi's replayer panic stops the update with an error,
// and leaves the backend answering, instead of holding its lock forever.
func TestAPanickingEntryStopsTheUpdate(t *testing.T) {
	s, _ := startOn(t)
	path, lease := begin(t, s, "update")
	unknown := int64(99)
	bad := apitype.JournalEntries{Entries: []apitype.JournalEntry{
		// DeleteNew names an operation the replayer never saw.
		{Version: 2, Kind: apitype.JournalEntryKindSuccess, SequenceID: 1, OperationID: 1, DeleteNew: &unknown},
	}}
	data, err := json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("PATCH", s.URL()+path+"/journalentries", bytes.NewReader(data))
	req.Header.Set("Authorization", lease)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("the request got no answer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("a panicking entry got %d, want 500", resp.StatusCode)
	}

	done := make(chan error, 1)
	go func() { done <- s.Save() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Save succeeded after the journal broke")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Save is still waiting: the lock was left held")
	}
	code, body := call(t, s, "GET", stackPath, user(s), nil, false)
	expect(t, "the stack after the panic", code, http.StatusOK, body)
}
