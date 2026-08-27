package golang

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/sst/sst/v3/cmd/sst/mosaic/ui/common"
	"github.com/sst/sst/v3/pkg/bus"
)

// A StdoutEvent is the one event the UI prints verbatim, without a case of its
// own. Announcing a finished build through it keeps this fork's patch inside
// this package: upstream's event types, its UI switch and the stream registry
// it decodes through are all left untouched, so a rebase onto a new SST has
// nothing here to conflict with.
//
// The cost is that the line has to be dressed here rather than by the printer.
// These are the styles ui.printEvent uses for a step that succeeded: a bold
// green bar and a dim label, eleven columns wide.
var (
	buildBar   = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	buildLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// reportBuilt announces that a handler finished compiling, and how long it
// took. Only successful builds are reported: a failed one is already spoken
// for, by the errors the build output carries back to the caller.
func reportBuilt(handler string, took time.Duration) {
	bus.Publish(&common.StdoutEvent{
		Line: buildBar.Render("|  ") +
			buildLabel.Render(fmt.Sprintf("%-11s ", "Built")) +
			fmt.Sprintf("%s (%.1fs)", strings.TrimPrefix(handler, "./"), took.Seconds()),
	})
}
