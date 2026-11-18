package journal

import (
	"bytes"
	"compress/gzip"
	"crypto/subtle"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pulumi/pulumi/pkg/v3/backend"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
)

// The slice of Pulumi Cloud's REST API that an update goes through, as its
// OpenAPI spec (https://api.pulumi.com/api/openapi/pulumi-spec.json) defines
// it: the same paths, request members, status codes and response members.
// Where the spec is silent, the reference is the client Pulumi's CLI uses,
// pkg/backend/httpstate/client.

// statusCancelled is in the spec's enum of update statuses, but not among
// the SDK's constants.
const statusCancelled apitype.UpdateStatus = "cancelled"

// leaseDuration is how long an update's lease lasts when it starts, and the
// longest a renewal may ask for. Pulumi's CLI renews every eighth of it.
const leaseDuration = 300 * time.Second

// update is one update of the stack, in the states the spec walks it through.
type update struct {
	id      string
	kind    apitype.UpdateKind
	status  apitype.UpdateStatus
	version int
	started time.Time
	// lease is the token the update's own calls authenticate with, valid
	// until expires unless renewed. A lease that runs out times the update out.
	lease    string
	expires  time.Time
	timedOut bool
	// applied holds the sequence IDs of the journal entries already applied,
	// so a batch the CLI sends again, which it does when a response is lost,
	// is not applied twice.
	applied map[int64]struct{}
	// verbatim is the sequence number of the last whole checkpoint taken.
	verbatim int
}

// inProgress reports whether u is running and its lease has not run out,
// marking it timed out when it has. Callers hold s.mu.
func (u *update) inProgress(now time.Time) bool {
	if u.status == apitype.StatusRunning && !u.timedOut && now.After(u.expires) {
		u.timedOut = true
	}
	return u.status == apitype.StatusRunning && !u.timedOut
}

// conflict is the reason the spec gives for a 409 on an update that is not in
// progress.
func (u *update) conflict() string {
	switch {
	case u.timedOut:
		return "The Update has already timed out"
	case u.status == apitype.StatusNotStarted:
		return "The Update has not started"
	case u.status == statusCancelled:
		return "The Update has been cancelled"
	default:
		return "The Update has already completed"
	}
}

func (s *Server) routes() http.Handler {
	const project = "/api/stacks/{org}/{project}"
	const stack = project + "/{stack}"
	const update = stack + "/{kind}/{update}"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/capabilities", s.userCall(s.capabilities))
	mux.HandleFunc("GET /api/user", s.userCall(s.user))
	mux.HandleFunc("GET /api/user/organizations/default", s.userCall(s.defaultOrg))
	mux.HandleFunc("HEAD "+project, s.userCall(s.projectExists))
	mux.HandleFunc("GET "+stack, s.userCall(s.getStack))
	mux.HandleFunc("GET "+stack+"/export", s.userCall(s.exportStack))
	mux.HandleFunc("POST "+stack+"/{kind}", s.userCall(s.createUpdate))
	mux.HandleFunc("POST "+update, s.userCall(s.startUpdate))
	mux.HandleFunc("POST "+update+"/renew_lease", s.updateCall(s.renewLease))
	mux.HandleFunc("PATCH "+update+"/journalentries", s.updateCall(s.journalEntries))
	mux.HandleFunc("PATCH "+update+"/checkpoint", s.updateCall(s.checkpoint))
	mux.HandleFunc("PATCH "+update+"/checkpointverbatim", s.updateCall(s.checkpointVerbatim))
	mux.HandleFunc("POST "+update+"/events", s.updateCall(s.event))
	mux.HandleFunc("POST "+update+"/events/batch", s.updateCall(s.events))
	mux.HandleFunc("POST "+update+"/complete", s.updateCall(s.complete))
	mux.HandleFunc("/", s.notServed)
	return mux
}

// Authentication. Pulumi's client sends `Authorization: token <t>` with the
// user's access token, and `update-token <t>` with an update's lease token on
// the calls the spec says require update token authentication.

// userCall serves a call made with the backend's access token.
func (s *Server) userCall(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !credentials(r, "token", s.token) {
			s.unauthorized(w, r)
			return
		}
		next(w, r)
	}
}

// updateCall serves a call about one update of the served stack, made with
// that update's lease token.
func (s *Server) updateCall(next func(http.ResponseWriter, *http.Request, *update)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sameStack(w, r) {
			return
		}
		u := s.findUpdate(r)
		if u == nil {
			s.error(w, http.StatusNotFound, "Update not found")
			return
		}
		s.mu.Lock()
		lease := u.lease
		s.mu.Unlock()
		if lease == "" || !credentials(r, "update-token", lease) {
			s.unauthorized(w, r)
			return
		}
		next(w, r, u)
	}
}

func credentials(r *http.Request, scheme, want string) bool {
	got, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	return ok && got == scheme && subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
}

func (s *Server) unauthorized(w http.ResponseWriter, r *http.Request) {
	s.log.Warn("unauthorized call", "method", r.Method, "path", r.URL.Path)
	s.error(w, http.StatusUnauthorized, "Unauthorized")
}

func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	// The deployment schema versions the replayer writes, which are all of
	// them. Without this the CLI would downgrade what it sends to version 3.
	config, err := jsonv2.Marshal(apitype.DeploymentSchemaVersionConfig{
		Version: apitype.DeploymentSchemaVersionLatest,
	}, jsonOptions)
	if err != nil {
		s.error(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.reply(w, http.StatusOK, apitype.CapabilitiesResponse{Capabilities: []apitype.APICapabilityConfig{{
		Capability:    apitype.DeploymentSchemaVersion,
		Version:       1,
		Configuration: config,
	}}})
}

// user is the spec's User: every required member, describing the backend.
func (s *Server) user(w http.ResponseWriter, r *http.Request) {
	s.reply(w, http.StatusOK, map[string]any{
		"id":                  "sst",
		"githubLogin":         "sst",
		"name":                "SST",
		"email":               "",
		"avatarUrl":           "",
		"hasMFA":              false,
		"identities":          []string{},
		"isManagedByMultiOrg": false,
		"isOrgManaged":        false,
		"organizations": []map[string]string{{
			"githubLogin": s.opts.Org,
			"name":        s.opts.Org,
			"avatarUrl":   "",
			"role":        "admin",
		}},
	})
}

func (s *Server) defaultOrg(w http.ResponseWriter, r *http.Request) {
	s.reply(w, http.StatusOK, apitype.GetDefaultOrganizationResponse{
		GitHubLogin: s.opts.Org,
		Messages:    []apitype.Message{},
	})
}

func (s *Server) projectExists(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("org") != s.opts.Org || r.PathValue("project") != s.opts.Project {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// sameStack answers 404 for any stack but the one being served.
func (s *Server) sameStack(w http.ResponseWriter, r *http.Request) bool {
	if r.PathValue("org") == s.opts.Org && r.PathValue("project") == s.opts.Project &&
		r.PathValue("stack") == s.opts.Stack {
		return true
	}
	s.error(w, http.StatusNotFound, "Stack not found")
	return false
}

func (s *Server) getStack(w http.ResponseWriter, r *http.Request) {
	if !s.sameStack(w, r) {
		return
	}
	s.mu.Lock()
	stack := apitype.Stack{
		ID:          s.stackID,
		OrgName:     s.opts.Org,
		ProjectName: s.opts.Project,
		StackName:   tokens.QName(s.opts.Stack),
		// A copy: the reply is encoded after the lock is released, and
		// starting an update writes to the tags.
		Tags:    maps.Clone(s.tags),
		Version: s.version,
	}
	if s.latest != nil {
		stack.ActiveUpdate = s.latest.id
		if s.latest.inProgress(time.Now()) {
			stack.CurrentOperation = &apitype.OperationStatus{
				Kind:    s.latest.kind,
				Author:  "sst",
				Started: s.latest.started.Unix(),
			}
		}
	}
	s.mu.Unlock()
	s.reply(w, http.StatusOK, stack)
}

func (s *Server) exportStack(w http.ResponseWriter, r *http.Request) {
	if !s.sameStack(w, r) {
		return
	}
	s.reply(w, http.StatusOK, s.state.export())
}

func (s *Server) findUpdate(r *http.Request) *update {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.updates[r.PathValue("update")]
	// Once an update exists, the CLI addresses it as an "update" whatever its
	// kind (getUpdatePath in its httpstate client): a refresh, a destroy or a
	// preview is created at its own kind and then started, renewed, journaled
	// and completed under /update/. The kind it was created with is accepted
	// too.
	kind := r.PathValue("kind")
	if u == nil || (kind != string(apitype.UpdateUpdate) && kind != string(u.kind)) {
		return nil
	}
	return u
}

func (s *Server) createUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.sameStack(w, r) {
		return
	}
	kind := apitype.UpdateKind(r.PathValue("kind"))
	switch kind {
	case apitype.UpdateUpdate, apitype.PreviewUpdate, apitype.RefreshUpdate, apitype.DestroyUpdate:
	default:
		s.notServed(w, r)
		return
	}
	var req apitype.UpdateProgramRequest
	if _, ok := s.decode(w, r, &req, "config", "description", "main", "metadata", "name", "options", "runtime"); !ok {
		return
	}
	u := &update{
		id:      uuid.NewString(),
		kind:    kind,
		status:  apitype.StatusNotStarted,
		applied: map[int64]struct{}{},
	}
	s.mu.Lock()
	s.updates[u.id] = u
	s.mu.Unlock()
	s.log.Info("update created", "kind", kind, "update", u.id)
	s.reply(w, http.StatusOK, apitype.UpdateProgramResponse{UpdateID: u.id})
}

func (s *Server) startUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.sameStack(w, r) {
		return
	}
	u := s.findUpdate(r)
	if u == nil {
		s.error(w, http.StatusNotFound, "Update not found")
		return
	}
	var req apitype.StartUpdateRequest
	if _, ok := s.decode(w, r, &req); !ok {
		return
	}
	// Journal versions 1 and 2 are the ones Pulumi's replayer reads. A CLI
	// that asks for neither, or has journaling turned off, gets 0 back and
	// sends whole checkpoints instead, which this backend also takes.
	journal := min(req.JournalVersion, apitype.LatestJournalVersion)

	s.mu.Lock()
	now := time.Now()
	if s.latest != nil && s.latest != u && s.latest.inProgress(now) {
		s.mu.Unlock()
		s.error(w, http.StatusConflict, "Another update is currently in progress.")
		return
	}
	if u.status != apitype.StatusNotStarted {
		s.mu.Unlock()
		s.error(w, http.StatusConflict, "The Update has already started")
		return
	}
	lease, err := newToken()
	if err != nil {
		s.mu.Unlock()
		s.error(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.version++
	u.status = apitype.StatusRunning
	u.version = s.version
	u.started = now
	u.lease = lease
	u.expires = now.Add(leaseDuration)
	for name, value := range req.Tags {
		s.tags[name] = value
	}
	s.latest = u
	s.mu.Unlock()

	s.log.Info("update started", "kind", u.kind, "update", u.id, "version", u.version, "journalVersion", journal)
	if journal == 0 {
		s.report.printf("Pulumi is not journaling: every step rewrites the whole state")
	}
	s.reply(w, http.StatusOK, apitype.StartUpdateResponse{
		Version:         u.version,
		Token:           lease,
		TokenExpiration: u.expires.Unix(),
		JournalVersion:  journal,
	})
}

// renewLease extends the lease. The token stays the same: the CLI keeps
// sending calls with the one it holds while a renewal is in flight, and
// those must not fail because it changed under them.
func (s *Server) renewLease(w http.ResponseWriter, r *http.Request, u *update) {
	var req apitype.RenewUpdateLeaseRequest
	if _, ok := s.decode(w, r, &req, "duration", "token"); !ok {
		return
	}
	if req.Duration < 0 || time.Duration(req.Duration)*time.Second > leaseDuration {
		s.error(w, http.StatusBadRequest, "Renewal duration must be between 0 and 300 seconds")
		return
	}
	s.mu.Lock()
	now := time.Now()
	if !u.inProgress(now) {
		reason := u.conflict()
		s.mu.Unlock()
		s.error(w, http.StatusConflict, reason)
		return
	}
	u.expires = now.Add(time.Duration(req.Duration) * time.Second)
	response := apitype.RenewUpdateLeaseResponse{Token: u.lease, TokenExpiration: u.expires.Unix()}
	s.mu.Unlock()
	s.reply(w, http.StatusOK, response)
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request, u *update) {
	var req apitype.CompleteUpdateRequest
	if _, ok := s.decode(w, r, &req, "status"); !ok {
		return
	}
	switch req.Status {
	case apitype.StatusNotStarted, apitype.StatusRequested, apitype.StatusRunning,
		apitype.StatusFailed, apitype.StatusSucceeded, statusCancelled:
	default:
		s.error(w, http.StatusBadRequest, "unrecognized status")
		return
	}
	s.mu.Lock()
	if !u.inProgress(time.Now()) {
		reason := u.conflict()
		s.mu.Unlock()
		s.error(w, http.StatusConflict, reason)
		return
	}
	s.mu.Unlock()
	// The state is final: write it before Pulumi exits, since SST reads it as
	// soon as Pulumi does. And before the update counts as completed, so a
	// save that failed can be tried again by the CLI's retry of this call.
	if err := s.flush(); err != nil {
		s.error(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mu.Lock()
	u.status = req.Status
	s.mu.Unlock()
	s.log.Info("update completed", "update", u.id, "status", req.Status)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) journalEntries(w http.ResponseWriter, r *http.Request, u *update) {
	start := time.Now()
	if !s.boolQuery(w, r, "include_non_activated") {
		return
	}
	var req struct {
		Entries []jsontext.Value `json:"entries"`
	}
	size, ok := s.decode(w, r, &req)
	if !ok {
		return
	}
	entries := make([]apitype.JournalEntry, len(req.Entries))
	for i, raw := range req.Entries {
		if err := s.unmarshalRequired(raw, &entries[i],
			"kind", "operationID", "removeNew", "removeOld", "sequenceID", "version"); err != nil {
			s.error(w, http.StatusBadRequest, fmt.Sprintf("entries[%d]: %v", i, err))
			return
		}
	}
	decoded := time.Now()

	s.mu.Lock()
	if !u.inProgress(decoded) {
		reason := u.conflict()
		s.mu.Unlock()
		s.error(w, http.StatusConflict, reason)
		return
	}
	err := s.failed
	repeated, skipped, changed := 0, 0, false
	for _, entry := range entries {
		if err != nil {
			break
		}
		if _, done := u.applied[entry.SequenceID]; done && entry.SequenceID != 0 {
			repeated++
			continue
		}
		unchanged := false
		err = guarded(func() error {
			unchanged = s.elide.unchanged(entry)
			if err := s.replayer.Add(entry); err != nil {
				return err
			}
			s.elide.record(entry)
			return nil
		})
		if err == nil {
			u.applied[entry.SequenceID] = struct{}{}
			if unchanged {
				skipped++
			} else {
				changed = true
			}
		}
	}
	applied := time.Now()
	if err == nil {
		// Entries that changed nothing are applied, but they do not make the
		// state worth saving on their own: see elision.
		if changed {
			s.dirty = true
		}
		if skipped > 0 {
			s.elided = true
		}
		s.stats.skipped += skipped
		s.stats.batches++
		s.stats.entries += len(entries) - repeated
		s.stats.bytesIn += size
		s.stats.applying += applied.Sub(start)
	}
	total := s.stats.entries
	s.mu.Unlock()

	if err != nil {
		// Acknowledging the batch would tell Pulumi its steps are safe when the
		// state cannot record them. An error stops the update instead.
		err = s.fail(fmt.Errorf("applying a journal entry: %w", err))
		s.error(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("journal entries", "count", len(entries), "repeated", repeated, "total", total, "bytes", size,
		"decode", decoded.Sub(start), "apply", applied.Sub(decoded))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) checkpoint(w http.ResponseWriter, r *http.Request, u *update) {
	var req apitype.PatchUpdateCheckpointRequest
	if _, ok := s.decode(w, r, &req, "isInvalid", "version"); !ok {
		return
	}
	if u.kind == apitype.PreviewUpdate {
		s.error(w, http.StatusForbidden, "Updating the checkpoint is not permitted for preview operations.")
		return
	}
	if req.IsInvalid {
		s.error(w, http.StatusBadRequest, "using the IsInvalid flag is not longer supported")
		return
	}
	if isNull(jsontext.Value(req.Deployment)) {
		s.error(w, http.StatusBadRequest, "request must contain a checkpoint")
		return
	}
	s.replaceDeployment(w, u, req.Deployment, 0)
}

func (s *Server) checkpointVerbatim(w http.ResponseWriter, r *http.Request, u *update) {
	var req apitype.PatchUpdateVerbatimCheckpointRequest
	if _, ok := s.decode(w, r, &req, "sequenceNumber", "version"); !ok {
		return
	}
	if u.kind == apitype.PreviewUpdate {
		s.error(w, http.StatusForbidden, "Updating the checkpoint is not permitted for preview operations.")
		return
	}
	s.mu.Lock()
	completed := !u.timedOut && u.status != apitype.StatusRunning && u.status != apitype.StatusNotStarted
	s.mu.Unlock()
	if completed {
		s.error(w, http.StatusBadRequest, "setting a checkpoint is not allowed because it's marked as already completed")
		return
	}
	var untyped apitype.UntypedDeployment
	if err := jsonv2.Unmarshal(req.UntypedDeployment, &untyped, jsonOptions); err != nil {
		s.error(w, http.StatusBadRequest, err.Error())
		return
	}
	s.replaceDeployment(w, u, untyped.Deployment, req.SequenceNumber)
}

// replaceDeployment makes a whole checkpoint the state, unless it is one the
// backend already has: a verbatim checkpoint whose sequence number is not
// past the last one taken, which is the CLI sending it again.
func (s *Server) replaceDeployment(w http.ResponseWriter, u *update, raw []byte, sequence int) {
	deployment := &apitype.DeploymentV3{}
	if err := jsonv2.Unmarshal(raw, deployment, jsonOptions); err != nil {
		s.error(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !u.inProgress(time.Now()) {
		s.error(w, http.StatusConflict, u.conflict())
		return
	}
	if sequence > 0 {
		if sequence <= u.verbatim {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		u.verbatim = sequence
	}
	s.replayer = backend.NewJournalReplayer(deployment)
	s.elide = newElision(deployment)
	s.dirty = true
	w.WriteHeader(http.StatusNoContent)
}

// event and events take the engine events, which Pulumi also writes to the
// event log SST reads them from. They are checked and dropped.
func (s *Server) event(w http.ResponseWriter, r *http.Request, u *update) {
	var event jsontext.Value
	if _, ok := s.decode(w, r, &event, "sequence", "timestamp"); !ok {
		return
	}
	s.countEvents(1)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, u *update) {
	var batch struct {
		Events []jsontext.Value `json:"events"`
	}
	if _, ok := s.decode(w, r, &batch, "events"); !ok {
		return
	}
	if len(batch.Events) == 0 {
		s.error(w, http.StatusBadRequest, "No events provided.")
		return
	}
	for i, event := range batch.Events {
		var v jsontext.Value
		if err := s.unmarshalRequired(event, &v, "sequence", "timestamp"); err != nil {
			s.error(w, http.StatusBadRequest, fmt.Sprintf("events[%d]: %v", i, err))
			return
		}
	}
	s.countEvents(len(batch.Events))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) countEvents(n int) {
	s.mu.Lock()
	s.stats.events += n
	s.mu.Unlock()
}

// notServed is every call this backend does not implement. Pulumi gets a 404
// it can report, and the log names the call, which is the place to look when
// a newer CLI starts making one.
func (s *Server) notServed(w http.ResponseWriter, r *http.Request) {
	s.log.Warn("endpoint not served", "method", r.Method, "path", r.URL.Path)
	s.error(w, http.StatusNotFound, fmt.Sprintf("%s %s is not served by SST's journaling backend", r.Method, r.URL.Path))
}

// decode reads a request body, gzipped or not (Pulumi compresses the large
// ones), checks it carries the members the spec marks as required, and
// decodes it into v. It reports how many bytes the body took on the wire, and
// answers 400 itself when the body is not what the spec asks for.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any, required ...string) (int64, bool) {
	counter := &countingReader{r: r.Body}
	var body io.Reader = counter
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(counter)
		if err != nil {
			s.error(w, http.StatusBadRequest, err.Error())
			return 0, false
		}
		// Closing a gzip reader only reports what reading it already did.
		defer func() { _ = gz.Close() }()
		body = gz
	}
	data, err := io.ReadAll(body)
	if err != nil {
		s.error(w, http.StatusBadRequest, err.Error())
		return 0, false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	if err := s.unmarshalRequired(data, v, required...); err != nil {
		s.error(w, http.StatusBadRequest, err.Error())
		return 0, false
	}
	return counter.n, true
}

// unmarshalRequired decodes a JSON object into v after checking it has every
// required member. A member that is present and null counts as present: the
// spec's required members are the ones the CLI always sends.
func (s *Server) unmarshalRequired(data []byte, v any, required ...string) error {
	if len(required) > 0 {
		var members map[string]jsontext.Value
		if err := jsonv2.Unmarshal(data, &members, jsonOptions); err != nil {
			return err
		}
		for _, name := range required {
			if _, ok := members[name]; !ok {
				return fmt.Errorf("missing required member %q", name)
			}
		}
	}
	return jsonv2.Unmarshal(data, v, jsonOptions)
}

// boolQuery answers 400 when a boolean query parameter is set to something
// that is not a boolean.
func (s *Server) boolQuery(w http.ResponseWriter, r *http.Request, name string) bool {
	value := r.URL.Query().Get(name)
	if value == "" {
		return true
	}
	if _, err := strconv.ParseBool(value); err != nil {
		s.error(w, http.StatusBadRequest, fmt.Sprintf("%s must be a boolean", name))
		return false
	}
	return true
}

func (s *Server) reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := jsonv2.MarshalWrite(w, v, jsonOptions); err != nil {
		s.log.Error("writing a response", "err", err)
	}
}

func (s *Server) error(w http.ResponseWriter, code int, message string) {
	s.reply(w, code, apitype.ErrorResponse{Code: code, Message: message})
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// guarded runs a call into Pulumi's replayer, which assumes well-formed entries
// and panics on some that are not. The panic comes back as an error, so the
// update stops on it, instead of leaving s.mu locked under net/http's own
// recover and every later call waiting on it.
func guarded(call func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return call()
}
