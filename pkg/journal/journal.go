// Package journal is a Pulumi state backend that keeps a journal.
//
// SST runs Pulumi against a DIY backend: a state file on disk, which SST pulls
// from its bucket before an update and pushes back while it runs. A DIY
// backend persists the state by rewriting the whole file after every resource
// it changes, one write at a time, so the cost of each step grows with the
// size of the stack. On a stack with ten thousand resources that is about two
// seconds per step, whatever the step does, and every parallel worker queues
// behind the same write.
//
// Pulumi Cloud does not pay this: the CLI sends it one small journal entry per
// step instead of the whole state. That protocol is Pulumi's public HTTP API,
// and nothing ties it to Pulumi Cloud, so this package serves the slice of it
// that `pulumi up`, `refresh` and `destroy` use, on 127.0.0.1, for the length
// of one update. Pulumi then journals. The entries are applied with Pulumi's
// own replayer, and the state file SST pushes is rebuilt from them whenever SST
// is about to push it, so what lands in the bucket, and when, is what landed
// there before.
package journal

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pulumi/pulumi/pkg/v3/backend"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
)

// Options say which state the backend serves.
type Options struct {
	// StatePath is the checkpoint file SST pulled, and the one it pushes.
	StatePath string
	// Org, Project and Stack name the stack as `--stack` does.
	Org, Project, Stack string
	// Dir is where Pulumi may keep the credentials it stores for this backend.
	// It should be the update's own working directory, which SST removes.
	Dir string
}

// Server is the backend for one update.
type Server struct {
	opts   Options
	token  string
	log    *slog.Logger
	report reporter
	ln     net.Listener
	srv    *http.Server
	start  time.Time

	// mu guards the journal (the replayer, what it was fed, and whether the
	// file on disk is behind it) and the stack as the API describes it.
	mu       sync.Mutex
	state    *state
	replayer *backend.JournalReplayer
	dirty    bool
	// elided is set when entries that changed nothing came since the last
	// save: only the final save writes them.
	elided  bool
	elide   *elision
	failed  error
	stats   stats
	stackID string
	version int
	tags    map[apitype.StackTagName]string
	updates map[string]*update
	latest  *update

	// saveMu lets one save run at a time, and owns buf.
	saveMu sync.Mutex
	buf    bytes.Buffer

	closeOnce sync.Once
}

type stats struct {
	batches  int
	entries  int
	bytesIn  int64
	applying time.Duration
	events   int
	skipped  int
	saves    int
	// savedAt is how many entries the last save had.
	savedAt int
	saving  time.Duration
	slowest time.Duration
}

// Start reads the state and starts serving it.
func Start(opts Options) (*Server, error) {
	s := &Server{
		opts:   opts,
		log:    slog.Default().With("service", "journal"),
		report: reporter{},
		start:  time.Now(),
		// The same stack always gets the same ID.
		stackID: uuid.NewSHA1(uuid.NameSpaceURL,
			[]byte(opts.Org+"/"+opts.Project+"/"+opts.Stack)).String(),
		tags:    map[apitype.StackTagName]string{},
		updates: map[string]*update{},
	}

	read := time.Now()
	st, err := readState(opts.StatePath)
	if err != nil {
		return nil, err
	}
	s.state = st
	s.replayer = backend.NewJournalReplayer(st.base)
	s.elide = newElision(st.base)
	took := time.Since(read)

	s.token, err = newToken()
	if err != nil {
		return nil, err
	}

	s.ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.srv = &http.Server{Handler: s.routes()}
	go func() {
		if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("backend stopped serving", "err", err)
		}
	}()

	s.log.Info("backend started", "url", s.URL(), "resources", len(st.base.Resources), "read", took)
	s.report.printf("backend on %s, %d resources read (%s)", s.ln.Addr(), len(st.base.Resources), seconds(took))
	return s, nil
}

func newToken() (string, error) {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return "", err
	}
	return hex.EncodeToString(token), nil
}

// URL is where the backend listens.
func (s *Server) URL() string {
	return "http://" + s.ln.Addr().String()
}

// Env is what points a Pulumi CLI at this backend. Appended after the DIY
// backend's own variables, these win.
func (s *Server) Env() []string {
	return []string{
		"PULUMI_BACKEND_URL=" + s.URL(),
		"PULUMI_ACCESS_TOKEN=" + s.token,
		// Pulumi stores the account it logs in with. Keep it out of the
		// shared PULUMI_HOME: the URL and the token change on every update.
		"PULUMI_CREDENTIALS_PATH=" + s.opts.Dir,
		// The secrets keep being encrypted with SST's passphrase, which the
		// state records. Without this an HTTP backend defaults to Pulumi
		// Cloud's own secrets manager.
		"PULUMI_FALLBACK_TO_STATE_SECRETS_MANAGER=true",
	}
}

// Save writes the state the journal has reached, if it moved since the last
// save. SST calls it before every push.
func (s *Server) Save() error {
	return s.save(false)
}

// flush writes the state the journal has reached when the update ends, the
// entries that changed nothing included, as Pulumi flushes its elided writes.
func (s *Server) flush() error {
	return s.save(true)
}

func (s *Server) save(final bool) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.Lock()
	if s.failed != nil {
		err := s.failed
		s.mu.Unlock()
		return err
	}
	if !s.dirty && !(final && s.elided) {
		s.mu.Unlock()
		return nil
	}
	entries, since := s.stats.entries, s.stats.entries-s.stats.savedAt
	s.stats.savedAt = entries
	start := time.Now()
	deployment, err := s.replayer.GenerateDeployment()
	s.dirty, s.elided = false, false
	s.mu.Unlock()
	if err != nil {
		return s.fail(fmt.Errorf("rebuilding the state: %w", err))
	}
	built := time.Now()

	// Encoding happens outside the lock, so Pulumi's next entries are not held
	// up by it. The deployment is a copy: the replayer changes its own
	// resources, never the ones it handed out.
	if err := s.state.encode(&s.buf, deployment); err != nil {
		return s.fail(fmt.Errorf("encoding the state: %w", err))
	}
	encoded := time.Now()
	if err := writeFile(s.opts.StatePath, s.buf.Bytes()); err != nil {
		return s.fail(fmt.Errorf("writing the state: %w", err))
	}
	written := time.Now()

	took := written.Sub(start)
	s.mu.Lock()
	s.stats.saves++
	s.stats.saving += took
	s.stats.slowest = max(s.stats.slowest, took)
	s.mu.Unlock()

	resources := len(deployment.Deployment.Resources)
	s.log.Info("state saved",
		"entries", entries, "new", since, "resources", resources, "bytes", s.buf.Len(),
		"build", built.Sub(start), "encode", encoded.Sub(built), "write", written.Sub(encoded))
	s.report.printf("saved %d resources, %s, after %d entries, %d new (build %s, encode %s, write %s)",
		resources, kilobytes(s.buf.Len()), entries, since,
		seconds(built.Sub(start)), seconds(encoded.Sub(built)), seconds(written.Sub(encoded)))
	return nil
}

// Close saves what is left and stops the backend. The update may have ended
// without telling the backend, killed or crashed: the state still gets
// everything the journal received. Only the first call does anything, and a
// save that fails is reported like any other.
func (s *Server) Close() {
	s.closeOnce.Do(s.close)
}

func (s *Server) close() {
	if err := s.flush(); err != nil {
		s.log.Error("saving the state on close", "err", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(ctx); err != nil {
		s.log.Warn("stopping the backend", "err", err)
	}

	s.mu.Lock()
	st := s.stats
	status := "not completed"
	if u := s.latest; u != nil {
		switch {
		case u.timedOut:
			status = "timed out"
		case u.status != apitype.StatusRunning && u.status != apitype.StatusNotStarted:
			status = string(u.status)
		}
	}
	s.mu.Unlock()
	s.log.Info("backend stopped",
		"status", status, "batches", st.batches, "entries", st.entries, "bytes", st.bytesIn,
		"applying", st.applying, "skipped", st.skipped, "events", st.events, "saves", st.saves, "saving", st.saving,
		"slowest", st.slowest, "total", time.Since(s.start))
	s.report.printf("%s: %s in %s, applied in %s, %d left the state as it was; %s in %s, slowest %s",
		status, count(st.entries, "entry", "entries"), count(st.batches, "batch", "batches"), seconds(st.applying),
		st.skipped, count(st.saves, "save", "saves"), seconds(st.saving), seconds(st.slowest))
}

func (s *Server) fail(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed == nil {
		s.failed = err
		s.log.Error("backend failed", "err", err)
		s.report.printf("failed: %v", err)
	}
	return s.failed
}
