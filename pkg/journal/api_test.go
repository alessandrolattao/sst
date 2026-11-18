package journal

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
)

// These tests hold the backend to Pulumi Cloud's OpenAPI spec for every call
// it serves: the members a request must carry, the status codes, and the
// members a response must carry.

const (
	stackPath = "/api/stacks/organization/bench/dev"
	stackURN  = resource.URN("urn:pulumi:dev::bench::pulumi:pulumi:Stack::bench-dev")
)

// program is an UpdateProgramRequest with every member the spec requires.
var program = map[string]any{
	"name": "bench", "runtime": "nodejs", "main": "", "description": "",
	"options": map[string]any{}, "config": map[string]any{}, "metadata": map[string]any{},
}

// startOn writes a checkpoint holding only the stack resource, with some
// config, and starts a backend on it.
func startOn(t *testing.T) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dev.json")
	deployment, err := json.Marshal(apitype.DeploymentV3{
		Resources: []apitype.ResourceV3{{URN: stackURN, Type: "pulumi:pulumi:Stack"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := json.Marshal(map[string]json.RawMessage{
		"stack":  json.RawMessage(`"organization/bench/dev"`),
		"config": json.RawMessage(`{"aws:region":"eu-west-1"}`),
		"latest": deployment,
	})
	if err != nil {
		t.Fatal(err)
	}
	file, err := json.Marshal(apitype.VersionedCheckpoint{Version: 3, Checkpoint: checkpoint})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, file, 0644); err != nil {
		t.Fatal(err)
	}
	server, err := Start(Options{StatePath: path, Org: "organization", Project: "bench", Stack: "dev", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return server, path
}

// call makes a request the way Pulumi's client does, with the Authorization
// header given whole, and a gzipped body when asked.
func call(t *testing.T, s *Server, method, path, auth string, body any, gzipped bool) (int, []byte) {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		var w io.Writer = &payload
		var gz *gzip.Writer
		if gzipped {
			gz = gzip.NewWriter(&payload)
			w = gz
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Fatal(err)
		}
		if gz != nil {
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	req, err := http.NewRequest(method, s.URL()+path, &payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func user(s *Server) string { return "token " + s.token }

// begin creates and starts an update of the given kind, as the CLI does, and
// returns its path and the Authorization its own calls need. Like the CLI, it
// creates the update at its kind and addresses it as an "update" from then on.
func begin(t *testing.T, s *Server, kind string) (string, string) {
	t.Helper()
	code, body := call(t, s, "POST", stackPath+"/"+kind, user(s), program, false)
	if code != http.StatusOK {
		t.Fatalf("creating the update: %d %s", code, body)
	}
	var created apitype.UpdateProgramResponse
	decodeInto(t, body, &created)
	path := stackPath + "/update/" + created.UpdateID
	code, body = call(t, s, "POST", path, user(s), apitype.StartUpdateRequest{JournalVersion: 2}, false)
	if code != http.StatusOK {
		t.Fatalf("starting the update: %d %s", code, body)
	}
	var started apitype.StartUpdateResponse
	decodeInto(t, body, &started)
	return path, "update-token " + started.Token
}

func decodeInto(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
}

// requireMembers fails when a JSON object lacks a member the spec requires.
func requireMembers(t *testing.T, what string, body []byte, names ...string) {
	t.Helper()
	var members map[string]json.RawMessage
	decodeInto(t, body, &members)
	for _, name := range names {
		if _, ok := members[name]; !ok {
			t.Errorf("%s has no %q, which the spec requires: %s", what, name, body)
		}
	}
}

func expect(t *testing.T, what string, code, want int, body []byte) {
	t.Helper()
	if code != want {
		t.Errorf("%s: got %d, want %d: %s", what, code, want, body)
	}
}

func bucket(i int) apitype.ResourceV3 {
	return apitype.ResourceV3{
		URN:    resource.URN(fmt.Sprintf("urn:pulumi:dev::bench::aws:s3/bucket:Bucket::b%d", i)),
		Type:   "aws:s3/bucket:Bucket",
		Custom: true, ID: resource.ID(fmt.Sprint(i)), Parent: stackURN,
		Dependencies: []resource.URN{stackURN},
		Outputs:      map[string]any{"arn": fmt.Sprintf("arn:aws:s3:::b%d", i)},
	}
}

// creation is the journal of an update that keeps the stack and creates the
// given buckets, with sequence IDs from 1.
func creation(buckets ...apitype.ResourceV3) apitype.JournalEntries {
	first := int64(0)
	stack := apitype.ResourceV3{URN: stackURN, Type: "pulumi:pulumi:Stack"}
	entries := []apitype.JournalEntry{
		{Version: 2, Kind: apitype.JournalEntryKindSuccess, SequenceID: 1, OperationID: 0, State: &stack, RemoveOld: &first},
	}
	for i := range buckets {
		op := int64(i + 1)
		entries = append(entries,
			apitype.JournalEntry{Version: 2, Kind: apitype.JournalEntryKindBegin, SequenceID: 2 * op, OperationID: op,
				Operation: &apitype.OperationV2{Resource: buckets[i], Type: apitype.OperationTypeCreating}},
			apitype.JournalEntry{Version: 2, Kind: apitype.JournalEntryKindSuccess, SequenceID: 2*op + 1, OperationID: op,
				State: &buckets[i]},
		)
	}
	return apitype.JournalEntries{Entries: entries}
}

func readLatest(t *testing.T, path string) *apitype.CheckpointV3 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var versioned apitype.VersionedCheckpoint
	decodeInto(t, data, &versioned)
	var checkpoint apitype.CheckpointV3
	decodeInto(t, versioned.Checkpoint, &checkpoint)
	return &checkpoint
}

func TestUserCallsTakeTheAccessTokenOnly(t *testing.T) {
	s, _ := startOn(t)
	for _, auth := range []string{"", "token wrong", "update-token " + s.token, s.token} {
		code, body := call(t, s, "GET", "/api/user", auth, nil, false)
		expect(t, fmt.Sprintf("GET /api/user with %q", auth), code, http.StatusUnauthorized, body)
	}
	code, body := call(t, s, "GET", "/api/user", user(s), nil, false)
	expect(t, "GET /api/user with the access token", code, http.StatusOK, body)
}

func TestUpdateCallsTakeTheLeaseTokenOnly(t *testing.T) {
	s, _ := startOn(t)
	path, lease := begin(t, s, "update")
	for _, auth := range []string{user(s), "token " + lease[len("update-token "):], "update-token wrong"} {
		code, body := call(t, s, "PATCH", path+"/journalentries", auth, apitype.JournalEntries{}, false)
		expect(t, fmt.Sprintf("journalentries with %q", auth), code, http.StatusUnauthorized, body)
	}
	code, body := call(t, s, "PATCH", path+"/journalentries", lease, apitype.JournalEntries{}, false)
	expect(t, "journalentries with the lease token", code, http.StatusNoContent, body)
}

func TestResponsesCarryTheRequiredMembers(t *testing.T) {
	s, _ := startOn(t)

	code, body := call(t, s, "GET", "/api/capabilities", user(s), nil, false)
	expect(t, "capabilities", code, http.StatusOK, body)
	requireMembers(t, "AppCapabilitiesResponse", body, "capabilities")
	var capabilities apitype.CapabilitiesResponse
	decodeInto(t, body, &capabilities)
	parsed, err := capabilities.Parse()
	if err != nil {
		t.Fatal(err)
	}
	if parsed.DeploymentSchemaVersion != apitype.DeploymentSchemaVersionLatest {
		t.Errorf("the backend supports deployment schema %d, want %d",
			parsed.DeploymentSchemaVersion, apitype.DeploymentSchemaVersionLatest)
	}

	code, body = call(t, s, "GET", "/api/user", user(s), nil, false)
	expect(t, "user", code, http.StatusOK, body)
	requireMembers(t, "User", body, "avatarUrl", "email", "githubLogin", "hasMFA", "id", "identities",
		"isManagedByMultiOrg", "isOrgManaged", "name", "organizations")
	var account struct{ Organizations []json.RawMessage }
	decodeInto(t, body, &account)
	if len(account.Organizations) != 1 {
		t.Fatalf("the user has %d organizations, want 1", len(account.Organizations))
	}
	requireMembers(t, "OrganizationSummaryWithRole", account.Organizations[0], "avatarUrl", "githubLogin", "name", "role")

	code, body = call(t, s, "GET", "/api/user/organizations/default", user(s), nil, false)
	expect(t, "default organization", code, http.StatusOK, body)
	requireMembers(t, "AppGetDefaultOrganizationResponse", body, "GitHubLogin", "Messages")

	code, body = call(t, s, "GET", stackPath, user(s), nil, false)
	expect(t, "stack", code, http.StatusOK, body)
	requireMembers(t, "AppStack", body, "activeUpdate", "id", "orgName", "projectName", "stackName", "version")

	code, body = call(t, s, "POST", stackPath+"/update", user(s), program, false)
	expect(t, "create", code, http.StatusOK, body)
	requireMembers(t, "AppUpdateProgramResponse", body, "updateID")
	var created apitype.UpdateProgramResponse
	decodeInto(t, body, &created)

	path := stackPath + "/update/" + created.UpdateID
	code, body = call(t, s, "POST", path, user(s), map[string]any{}, false)
	expect(t, "start", code, http.StatusOK, body)
	requireMembers(t, "AppStartUpdateResponse", body, "version")
	var started apitype.StartUpdateResponse
	decodeInto(t, body, &started)

	code, body = call(t, s, "POST", path+"/renew_lease", "update-token "+started.Token,
		apitype.RenewUpdateLeaseRequest{Duration: 60}, false)
	expect(t, "renew", code, http.StatusOK, body)
	requireMembers(t, "AppRenewUpdateLeaseResponse", body, "token")

	// While the update runs, the stack names it and says what is in flight.
	code, body = call(t, s, "GET", stackPath, user(s), nil, false)
	expect(t, "stack while updating", code, http.StatusOK, body)
	var stack apitype.Stack
	decodeInto(t, body, &stack)
	if stack.ActiveUpdate != created.UpdateID || stack.CurrentOperation == nil || stack.Version != started.Version {
		t.Errorf("the stack during the update is %+v", stack)
	}
}

func TestRequestsMissingARequiredMemberAreRejected(t *testing.T) {
	s, _ := startOn(t)
	incomplete := map[string]any{}
	for name, value := range program {
		if name != "runtime" {
			incomplete[name] = value
		}
	}
	code, body := call(t, s, "POST", stackPath+"/update", user(s), incomplete, false)
	expect(t, "create without runtime", code, http.StatusBadRequest, body)

	path, lease := begin(t, s, "update")
	code, body = call(t, s, "POST", path+"/renew_lease", lease, map[string]any{"duration": 60}, false)
	expect(t, "renew without token", code, http.StatusBadRequest, body)

	entry := map[string]any{"version": 2, "kind": 0, "operationID": 1, "removeNew": nil, "removeOld": nil}
	code, body = call(t, s, "PATCH", path+"/journalentries", lease, map[string]any{"entries": []any{entry}}, false)
	expect(t, "journal entry without sequenceID", code, http.StatusBadRequest, body)

	code, body = call(t, s, "PATCH", path+"/checkpoint", lease, map[string]any{"version": 3}, false)
	expect(t, "checkpoint without isInvalid", code, http.StatusBadRequest, body)

	code, body = call(t, s, "PATCH", path+"/checkpointverbatim", lease, map[string]any{"version": 3}, false)
	expect(t, "verbatim checkpoint without sequenceNumber", code, http.StatusBadRequest, body)

	code, body = call(t, s, "POST", path+"/events/batch", lease, map[string]any{}, false)
	expect(t, "event batch without events", code, http.StatusBadRequest, body)

	code, body = call(t, s, "POST", path+"/events", lease, map[string]any{"sequence": 1}, false)
	expect(t, "event without timestamp", code, http.StatusBadRequest, body)

	code, body = call(t, s, "POST", path+"/complete", lease, map[string]any{}, false)
	expect(t, "complete without status", code, http.StatusBadRequest, body)
}

func TestStatusCodesOfAnUpdate(t *testing.T) {
	s, path := startOn(t)
	update, lease := begin(t, s, "update")

	code, body := call(t, s, "POST", update, user(s), map[string]any{}, false)
	expect(t, "starting it again", code, http.StatusConflict, body)

	code, body = call(t, s, "POST", stackPath+"/update/no-such-update", user(s), map[string]any{}, false)
	expect(t, "starting an update that does not exist", code, http.StatusNotFound, body)

	code, body = call(t, s, "POST", stackPath+"/update", user(s), program, false)
	expect(t, "creating a second update", code, http.StatusOK, body)
	var second apitype.UpdateProgramResponse
	decodeInto(t, body, &second)
	code, body = call(t, s, "POST", stackPath+"/update/"+second.UpdateID, user(s), map[string]any{}, false)
	expect(t, "starting it while the first runs", code, http.StatusConflict, body)

	code, body = call(t, s, "PATCH", update+"/journalentries", lease, creation(bucket(0)), true)
	expect(t, "journal entries", code, http.StatusNoContent, body)

	code, body = call(t, s, "PATCH", update+"/journalentries?include_non_activated=maybe", lease, apitype.JournalEntries{}, false)
	expect(t, "include_non_activated that is not a boolean", code, http.StatusBadRequest, body)

	event := map[string]any{"sequence": 1, "timestamp": 1}
	code, body = call(t, s, "POST", update+"/events/batch", lease, map[string]any{"events": []any{event}}, true)
	expect(t, "event batch", code, http.StatusOK, body)
	code, body = call(t, s, "POST", update+"/events/batch", lease, map[string]any{"events": []any{}}, false)
	expect(t, "empty event batch", code, http.StatusBadRequest, body)
	code, body = call(t, s, "POST", update+"/events", lease, event, false)
	expect(t, "single event", code, http.StatusOK, body)

	for _, duration := range []int{-1, 301} {
		code, body = call(t, s, "POST", update+"/renew_lease", lease,
			map[string]any{"token": "", "duration": duration}, false)
		expect(t, fmt.Sprintf("renewing for %ds", duration), code, http.StatusBadRequest, body)
	}

	code, body = call(t, s, "POST", update+"/complete", lease, map[string]any{"status": "done"}, false)
	expect(t, "completing with an unrecognized status", code, http.StatusBadRequest, body)
	code, body = call(t, s, "POST", update+"/complete", lease, apitype.CompleteUpdateRequest{Status: apitype.StatusSucceeded}, false)
	expect(t, "complete", code, http.StatusNoContent, body)

	// Completed: what needs an update in progress conflicts, and a verbatim
	// checkpoint says it is already completed.
	code, body = call(t, s, "POST", update+"/complete", lease, apitype.CompleteUpdateRequest{Status: apitype.StatusSucceeded}, false)
	expect(t, "completing it again", code, http.StatusConflict, body)
	code, body = call(t, s, "PATCH", update+"/journalentries", lease, apitype.JournalEntries{}, false)
	expect(t, "journal entries once completed", code, http.StatusConflict, body)
	code, body = call(t, s, "POST", update+"/renew_lease", lease, map[string]any{"token": "", "duration": 60}, false)
	expect(t, "renewing once completed", code, http.StatusConflict, body)
	code, body = call(t, s, "PATCH", update+"/checkpointverbatim", lease,
		map[string]any{"version": 3, "sequenceNumber": 9, "untypedDeployment": map[string]any{}}, false)
	expect(t, "verbatim checkpoint once completed", code, http.StatusBadRequest, body)

	if latest := readLatest(t, path).Latest; len(latest.Resources) != 2 {
		t.Errorf("the state has %d resources after the update, want the stack and the bucket", len(latest.Resources))
	}
}

func TestALeaseThatRunsOutTimesTheUpdateOut(t *testing.T) {
	s, _ := startOn(t)
	update, lease := begin(t, s, "update")
	s.mu.Lock()
	s.latest.expires = time.Now().Add(-time.Second)
	s.mu.Unlock()

	code, body := call(t, s, "PATCH", update+"/journalentries", lease, creation(bucket(0)), false)
	expect(t, "journal entries after the lease ran out", code, http.StatusConflict, body)
	code, body = call(t, s, "POST", update+"/renew_lease", lease, map[string]any{"token": "", "duration": 60}, false)
	expect(t, "renewing after the lease ran out", code, http.StatusConflict, body)
	code, body = call(t, s, "POST", update+"/complete", lease, apitype.CompleteUpdateRequest{Status: apitype.StatusSucceeded}, false)
	expect(t, "completing after the lease ran out", code, http.StatusConflict, body)
}

func TestARenewalKeepsTheLeaseGoing(t *testing.T) {
	s, _ := startOn(t)
	update, lease := begin(t, s, "update")
	s.mu.Lock()
	s.latest.expires = time.Now().Add(2 * time.Second)
	s.mu.Unlock()

	code, body := call(t, s, "POST", update+"/renew_lease", lease, map[string]any{"token": "", "duration": 300}, false)
	expect(t, "renew", code, http.StatusOK, body)
	var renewed apitype.RenewUpdateLeaseResponse
	decodeInto(t, body, &renewed)
	if "update-token "+renewed.Token != lease {
		t.Error("the renewal changed the token: calls already in flight with the old one would fail")
	}
	if until := time.Until(time.Unix(renewed.TokenExpiration, 0)); until < 290*time.Second {
		t.Errorf("the lease ends in %s after renewing it for 300s", until)
	}
}

func TestEntriesReachTheFile(t *testing.T) {
	s, path := startOn(t)
	update, lease := begin(t, s, "update")
	code, body := call(t, s, "PATCH", update+"/journalentries", lease, creation(bucket(0)), true)
	expect(t, "journal entries", code, http.StatusNoContent, body)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	checkpoint := readLatest(t, path)
	if checkpoint.Stack != "organization/bench/dev" {
		t.Errorf("the stack became %q", checkpoint.Stack)
	}
	if len(checkpoint.Config) != 1 {
		t.Errorf("the config became %v", checkpoint.Config)
	}
	latest := checkpoint.Latest
	if len(latest.Resources) != 2 || latest.Resources[0].URN != stackURN || latest.Resources[1].URN != bucket(0).URN {
		t.Fatalf("the resources are %+v, want the stack and then the bucket", latest.Resources)
	}
	if latest.Resources[1].Outputs["arn"] != "arn:aws:s3:::b0" {
		t.Errorf("the bucket's outputs are %v", latest.Resources[1].Outputs)
	}
	if len(latest.PendingOperations) != 0 {
		t.Errorf("pending operations left: %+v", latest.PendingOperations)
	}

	// The export still serves the state the update started from: the journal
	// entries' indices point into that one.
	code, body = call(t, s, "GET", stackPath+"/export", user(s), nil, false)
	expect(t, "export", code, http.StatusOK, body)
	var exported apitype.UntypedDeployment
	decodeInto(t, body, &exported)
	var base apitype.DeploymentV3
	decodeInto(t, exported.Deployment, &base)
	if len(base.Resources) != 1 {
		t.Errorf("the export has %d resources, want the stack alone", len(base.Resources))
	}
}

// TestABatchSentTwiceIsAppliedOnce: Pulumi's client sends a batch again when
// it gets no answer, and the spec's journal is idempotent for it.
func TestABatchSentTwiceIsAppliedOnce(t *testing.T) {
	s, path := startOn(t)
	update, lease := begin(t, s, "update")
	batch := creation(bucket(0), bucket(1))
	for range 2 {
		code, body := call(t, s, "PATCH", update+"/journalentries", lease, batch, false)
		expect(t, "journal entries", code, http.StatusNoContent, body)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if got := len(readLatest(t, path).Latest.Resources); got != 3 {
		t.Fatalf("the state has %d resources, want the stack and two buckets once", got)
	}
}

func TestWholeCheckpoints(t *testing.T) {
	s, path := startOn(t)
	update, lease := begin(t, s, "update")
	deployment := apitype.DeploymentV3{Resources: []apitype.ResourceV3{
		{URN: stackURN, Type: "pulumi:pulumi:Stack"}, bucket(0),
	}}

	code, body := call(t, s, "PATCH", update+"/checkpoint", lease,
		map[string]any{"isInvalid": true, "version": 3}, false)
	expect(t, "checkpoint marked invalid", code, http.StatusBadRequest, body)
	code, body = call(t, s, "PATCH", update+"/checkpoint", lease,
		map[string]any{"isInvalid": false, "version": 3}, false)
	expect(t, "checkpoint without a deployment", code, http.StatusBadRequest, body)
	code, body = call(t, s, "PATCH", update+"/checkpoint", lease,
		map[string]any{"isInvalid": false, "version": 3, "deployment": deployment}, false)
	expect(t, "checkpoint", code, http.StatusNoContent, body)

	// A verbatim checkpoint older than the last one taken is the CLI sending
	// it again: it changes nothing.
	verbatim := func(sequence int, deployment apitype.DeploymentV3) {
		t.Helper()
		code, body := call(t, s, "PATCH", update+"/checkpointverbatim", lease, map[string]any{
			"version": 3, "sequenceNumber": sequence,
			"untypedDeployment": map[string]any{"version": 3, "deployment": deployment},
		}, false)
		expect(t, fmt.Sprintf("verbatim checkpoint %d", sequence), code, http.StatusNoContent, body)
	}
	two := deployment
	two.Resources = append(append([]apitype.ResourceV3{}, deployment.Resources...), bucket(1))
	verbatim(2, two)
	verbatim(1, deployment)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if got := len(readLatest(t, path).Latest.Resources); got != 3 {
		t.Fatalf("the state has %d resources, want the 3 of checkpoint 2", got)
	}

	code, body = call(t, s, "POST", update+"/complete", lease, apitype.CompleteUpdateRequest{Status: apitype.StatusSucceeded}, false)
	expect(t, "complete", code, http.StatusNoContent, body)
	preview, previewLease := begin(t, s, "preview")
	code, body = call(t, s, "PATCH", preview+"/checkpoint", previewLease,
		map[string]any{"isInvalid": false, "version": 3, "deployment": deployment}, false)
	expect(t, "checkpoint of a preview", code, http.StatusForbidden, body)
}

// A whole checkpoint, which Pulumi sends when it does not journal, is saved
// as it was sent: with every pending operation, not only the creates.
func TestACheckpointIsSavedAsSent(t *testing.T) {
	s, statePath := startOn(t)
	code, body := call(t, s, "POST", stackPath+"/update", user(s), program, false)
	expect(t, "create", code, http.StatusOK, body)
	var created apitype.UpdateProgramResponse
	decodeInto(t, body, &created)
	path := stackPath + "/update/" + created.UpdateID
	code, body = call(t, s, "POST", path, user(s), apitype.StartUpdateRequest{JournalVersion: 0}, false)
	expect(t, "start", code, http.StatusOK, body)
	var started apitype.StartUpdateResponse
	decodeInto(t, body, &started)

	b := bucket(0)
	deployment, err := json.Marshal(apitype.DeploymentV3{
		Resources: []apitype.ResourceV3{{URN: stackURN, Type: "pulumi:pulumi:Stack"}, b},
		PendingOperations: []apitype.OperationV2{
			{Resource: b, Type: apitype.OperationTypeUpdating},
			{Resource: bucket(1), Type: apitype.OperationTypeCreating},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	code, body = call(t, s, "PATCH", path+"/checkpoint", "update-token "+started.Token,
		apitype.PatchUpdateCheckpointRequest{Version: 3, Deployment: deployment}, true)
	expect(t, "checkpoint", code, http.StatusNoContent, body)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	var types []apitype.OperationType
	for _, op := range readLatest(t, statePath).Latest.PendingOperations {
		types = append(types, op.Type)
	}
	if want := []apitype.OperationType{apitype.OperationTypeUpdating, apitype.OperationTypeCreating}; !slices.Equal(types, want) {
		t.Errorf("pending operations in the file: %v, want %v", types, want)
	}
}

// TestEntryItCannotApplyStopsTheUpdate: a step the state cannot record must
// not be acknowledged, and the file keeps the state it had.
func TestEntryItCannotApplyStopsTheUpdate(t *testing.T) {
	s, path := startOn(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	update, lease := begin(t, s, "update")
	bad := apitype.JournalEntries{Entries: []apitype.JournalEntry{{Version: 99, Kind: apitype.JournalEntryKindBegin, SequenceID: 1}}}
	code, body := call(t, s, "PATCH", update+"/journalentries", lease, bad, false)
	expect(t, "an entry the replayer refuses", code, http.StatusInternalServerError, body)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the state file changed after a refused entry")
	}
}

func TestOnlyTheServedStackAndProjectExist(t *testing.T) {
	s, _ := startOn(t)
	code, body := call(t, s, "GET", "/api/stacks/organization/bench/prod", user(s), nil, false)
	expect(t, "another stack", code, http.StatusNotFound, body)
	code, _ = call(t, s, "HEAD", "/api/stacks/organization/bench", user(s), nil, false)
	expect(t, "the served project", code, http.StatusOK, nil)
	code, _ = call(t, s, "HEAD", "/api/stacks/organization/other", user(s), nil, false)
	expect(t, "another project", code, http.StatusNotFound, nil)
}

// TestSavesWhileEntriesArrive is what an SST deploy does all along: Pulumi
// sends entries while SST saves the state before each push. Run with -race,
// it checks that a save reads nothing the next entries change.
func TestSavesWhileEntriesArrive(t *testing.T) {
	s, path := startOn(t)
	update, lease := begin(t, s, "update")
	const batches = 50
	all := make([]apitype.ResourceV3, batches)
	for i := range all {
		all[i] = bucket(i)
	}
	entries := creation(all...).Entries
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The stack's entry and the first creation, then one creation (its
		// begin and its success) per batch.
		batch := entries[:3]
		for i := range batches {
			code, body := call(t, s, "PATCH", update+"/journalentries", lease,
				apitype.JournalEntries{Entries: batch}, i%2 == 0)
			if code != http.StatusNoContent {
				t.Errorf("batch %d: %d %s", i, code, body)
				return
			}
			if i+1 < batches {
				batch = entries[3+2*i : 5+2*i]
			}
		}
	}()
	for {
		select {
		case <-done:
			if err := s.Save(); err != nil {
				t.Fatal(err)
			}
			if got := len(readLatest(t, path).Latest.Resources); got != batches+1 {
				t.Fatalf("the state has %d resources, want %d", got, batches+1)
			}
			return
		default:
			if err := s.Save(); err != nil {
				t.Fatal(err)
			}
		}
	}
}
