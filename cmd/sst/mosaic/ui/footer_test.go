package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/sst/sst/v3/pkg/project"
)

func deployingFooter(inFlight int) *footer {
	f := NewFooter()
	f.Update(&project.StackCommandEvent{Command: "deploy"})
	for i := range inFlight {
		f.Update(&apitype.ResourcePreEvent{Metadata: apitype.StepEventMetadata{
			Op:   apitype.OpCreate,
			URN:  fmt.Sprintf("urn:pulumi:ale::app::aws:lambda:Function::Handler%d", i),
			Type: "aws:lambda:Function",
		}})
	}
	return f
}

func TestFitHeightKeepsAFooterThatFits(t *testing.T) {
	view := deployingFooter(10).View(200)
	if got := fitHeight(view, 40); got != view {
		t.Errorf("footer of %d lines changed on a 40-row terminal:\n%s", strings.Count(view, "\n")+1, got)
	}
}

func TestFitHeightCutsAFooterTallerThanTheTerminal(t *testing.T) {
	view := deployingFooter(500).View(200)
	lines := strings.Split(fitHeight(view, 40), "\n")
	if len(lines) != 39 {
		t.Fatalf("got %d lines on a 40-row terminal, want 39", len(lines))
	}
	if !strings.Contains(lines[0], "Creating") || !strings.Contains(lines[0], "Handler0 ") {
		t.Errorf("first line is not the first resource in flight: %q", lines[0])
	}
	if !strings.Contains(lines[38], "Deploying") {
		t.Errorf("status line is not last: %q", lines[38])
	}
}
