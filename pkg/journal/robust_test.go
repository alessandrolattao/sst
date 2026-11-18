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

// send is a journal call that waits at most 5 seconds for its answer, and
// can be made outside the test's goroutine.
func send(s *Server, path, lease string, body any) (int, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest("PATCH", s.URL()+path, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", lease)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	return resp.StatusCode, resp.Body.Close()
}

// readOnly makes the state's folder read-only for the duration of a call, as
// a full disk or a lost permission would.
func readOnly(t *testing.T, statePath string, call func()) {
	t.Helper()
	dir := filepath.Dir(statePath)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Error(err)
		}
	}()
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
	code, err := send(s, path+"/journalentries", lease, bad)
	if err != nil {
		t.Fatalf("the request got no answer: %v", err)
	}
	if code != http.StatusInternalServerError {
		t.Errorf("a panicking entry got %d, want 500", code)
	}

	done := make(chan error, 1)
	go func() { done <- s.Save() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Save after the panic: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Save is still waiting: the lock was left held")
	}
	code, body := call(t, s, "GET", stackPath, user(s), nil, false)
	expect(t, "the stack after the panic", code, http.StatusOK, body)
}

// After a save that failed, a batch is acknowledged only once a save holds
// it: Pulumi stops, instead of going on with steps the state cannot record.
func TestEntriesWaitForAFailedSave(t *testing.T) {
	s, statePath := startOn(t)
	path, lease := begin(t, s, "update")
	second := bucket(1)
	more := apitype.JournalEntries{Entries: []apitype.JournalEntry{
		{Version: 2, Kind: apitype.JournalEntryKindBegin, SequenceID: 4, OperationID: 2,
			Operation: &apitype.OperationV2{Resource: second, Type: apitype.OperationTypeCreating}},
		{Version: 2, Kind: apitype.JournalEntryKindSuccess, SequenceID: 5, OperationID: 2, State: &second},
	}}
	readOnly(t, statePath, func() {
		code, body := call(t, s, "PATCH", path+"/journalentries", lease, creation(bucket(0)), false)
		expect(t, "the first bucket", code, http.StatusNoContent, body)
		if err := s.Save(); err == nil {
			t.Fatal("a save into a read-only folder succeeded")
		}
		code, body = call(t, s, "PATCH", path+"/journalentries", lease, more, false)
		expect(t, "a batch while the state cannot be written", code, http.StatusInternalServerError, body)
	})

	// The CLI sends the batch again.
	code, body := call(t, s, "PATCH", path+"/journalentries", lease, more, false)
	expect(t, "the batch sent again", code, http.StatusNoContent, body)
	if got := bucketIDs(t, statePath); !slices.Equal(got, []resource.ID{bucket(0).ID, bucket(1).ID}) {
		t.Errorf("buckets in the file: %v, want both", got)
	}
}

// An entry the replayer refuses stops the update, and the steps acknowledged
// before it are still saved.
func TestABrokenJournalKeepsWhatItAcknowledged(t *testing.T) {
	s, statePath := startOn(t)
	path, lease := begin(t, s, "update")
	code, body := call(t, s, "PATCH", path+"/journalentries", lease, creation(bucket(0)), false)
	expect(t, "an acknowledged bucket", code, http.StatusNoContent, body)
	bad := apitype.JournalEntries{Entries: []apitype.JournalEntry{
		{Version: 9, Kind: apitype.JournalEntryKindBegin, SequenceID: 10, OperationID: 5},
	}}
	code, body = call(t, s, "PATCH", path+"/journalentries", lease, bad, false)
	expect(t, "an entry the replayer refuses", code, http.StatusInternalServerError, body)
	code, body = call(t, s, "PATCH", path+"/journalentries", lease, creation(bucket(1)), false)
	expect(t, "a batch after the journal broke", code, http.StatusInternalServerError, body)

	code, body = call(t, s, "POST", path+"/complete", lease, map[string]any{"status": "failed"}, false)
	expect(t, "completing the failed update", code, http.StatusNoContent, body)
	if got := bucketIDs(t, statePath); !slices.Equal(got, []resource.ID{bucket(0).ID}) {
		t.Errorf("buckets in the file: %v, want the acknowledged one", got)
	}
	s.Close()
	if err := s.Err(); err != nil {
		t.Errorf("Err after a final save that went through: %v", err)
	}
}

// A batch that makes the replayer rebuild the state waits for a save that is
// encoding, since the rebuild rewrites in place what the save's deployment
// shares with the replayer. Any other batch goes through meanwhile.
func TestARebuildWaitsForTheSave(t *testing.T) {
	s, _ := startOn(t)
	path, lease := begin(t, s, "refresh")
	s.saveMu.Lock() // a save, encoding
	code, body := call(t, s, "PATCH", path+"/journalentries", lease, creation(bucket(0)), false)
	expect(t, "a batch during the save", code, http.StatusNoContent, body)

	rebuilt := apitype.JournalEntries{Entries: []apitype.JournalEntry{
		{Version: 2, Kind: apitype.JournalEntryKindRebuiltBaseState, SequenceID: 50},
	}}
	done := make(chan int, 1)
	go func() {
		code, err := send(s, path+"/journalentries", lease, rebuilt)
		if err != nil {
			code = 0
		}
		done <- code
	}()
	select {
	case code := <-done:
		s.saveMu.Unlock()
		t.Fatalf("the rebuild was applied during the save, with %d", code)
	case <-time.After(200 * time.Millisecond):
	}
	s.saveMu.Unlock()
	if code := <-done; code != http.StatusNoContent {
		t.Errorf("the rebuild after the save got %d, want 204", code)
	}
}
