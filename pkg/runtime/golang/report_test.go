package golang

import (
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
