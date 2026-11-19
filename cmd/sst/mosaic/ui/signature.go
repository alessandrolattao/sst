package ui

import (
	"fmt"

	"github.com/sst/sst/v3/pkg/runtime"
)

// shortSignature is enough of a signature to tell two apart at a glance. The
// whole of it is in the log.
func shortSignature(signature string) string {
	if len(signature) > 8 {
		return signature[:8]
	}
	return signature
}

// signatureNote says, after a build, what its signature is and why the
// deployed zip was not kept. A build that could not be signed says so only
// when asked to, since most runtimes never sign.
func signatureNote(evt *runtime.BuildEvent, verbose bool) string {
	if evt.Signature == "" {
		if verbose && evt.Reason != "" {
			return ", " + evt.Reason
		}
		return ""
	}
	if evt.Replaces != "" && evt.Replaces != evt.Signature {
		return fmt.Sprintf(" signature %s → %s", shortSignature(evt.Replaces), shortSignature(evt.Signature))
	}
	if evt.Reason == "" {
		return " signature " + shortSignature(evt.Signature)
	}
	return fmt.Sprintf(" signature %s, %s", shortSignature(evt.Signature), evt.Reason)
}

func unchangedSummary(count int) string {
	if count == 1 {
		return "1 function, its signature matches the deployed one"
	}
	return fmt.Sprintf("%d functions, signatures match the deployed ones", count)
}
