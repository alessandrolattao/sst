package golang

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/sst/sst/v3/cmd/sst/mosaic/ui/common"
	"github.com/sst/sst/v3/pkg/bus"
)

func TestReportBuilt(t *testing.T) {
	events := bus.Subscribe(&common.StdoutEvent{})

	reportBuilt("./services/search/query", 3210*time.Millisecond)

	select {
	case evt := <-events:
		line, ok := evt.(*common.StdoutEvent)
		if !ok {
			t.Fatalf("expected a StdoutEvent, got %T", evt)
		}
		// Colours depend on whether the test runs against a terminal, so the
		// assertion is on the text the UI ends up showing either way.
		got := ansi.Strip(line.Line)
		expected := "|  Built       services/search/query (3.2s)"
		if got != expected {
			t.Errorf("expected %q, got %q", expected, got)
		}
	case <-time.After(time.Second):
		t.Fatal("no build line published")
	}
}

// The line is only worth anything if a real build actually emits one, so this
// compiles a handler the way a deploy does and waits for its announcement.
func TestBuildReportsWhatItTook(t *testing.T) {
	requireGoToolchain(t)

	cfgDir := t.TempDir()
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "go.mod"), "module example.test\n\ngo 1.22\n")
	mustWriteFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")

	events := bus.Subscribe(&common.StdoutEvent{})
	buildBootstrap(t, cfgDir, dir, "fn-reported")

	// Every other test in this package builds too, so take the first line that
	// names this handler rather than the first line published.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case evt := <-events:
			line := ansi.Strip(evt.(*common.StdoutEvent).Line)
			if !strings.Contains(line, dir) {
				continue
			}
			if !regexp.MustCompile(`^\|  Built\s+\S+ \(\d+\.\d+s\)$`).MatchString(line) {
				t.Fatalf("unexpected build line: %q", line)
			}
			return
		case <-deadline:
			t.Fatal("the build published no line naming its handler")
		}
	}
}
