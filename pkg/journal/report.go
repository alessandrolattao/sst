package journal

import (
	"fmt"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/sst/sst/v3/cmd/sst/mosaic/ui/common"
	"github.com/sst/sst/v3/pkg/bus"
)

// LogEvent is a line for the "Pulumi Backend" pane of `sst dev`. It is a type
// of its own so that pane can ask for it alone, and the SST pane, which prints
// every StdoutEvent, stays as it was.
type LogEvent struct {
	Line string
}

// The styles ui.printEvent uses for a step that succeeded, the same ones the
// Go runtime's `Built` line borrows: a bold green bar and a dim label, eleven
// columns wide. A deploy prints these lines among its own, so they line up.
var (
	reportBar   = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	reportLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

const reportName = "Journal"

// reporter prints what the backend does. Outside dev, where the deploy output
// is all there is (a CI log, a terminal), a line goes out as a StdoutEvent,
// which the UI prints verbatim. In dev it goes to the backend's own pane.
type reporter struct {
	dev bool
}

func (r reporter) printf(format string, args ...any) {
	line := reportBar.Render("|  ") +
		reportLabel.Render(fmt.Sprintf("%-11s ", reportName)) +
		fmt.Sprintf(format, args...)
	if r.dev {
		bus.Publish(&LogEvent{Line: line})
		return
	}
	bus.Publish(&common.StdoutEvent{Line: line})
}

func seconds(d time.Duration) string {
	return fmt.Sprintf("%.2fs", d.Seconds())
}

func megabytes(n int) string {
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
