package journal

import (
	"fmt"
	"time"

	"github.com/sst/sst/v3/pkg/bus"
)

// ReportEvent is a line about what the backend did. The deploy output prints
// it among its own lines, and `sst dev` gives it a pane of its own.
type ReportEvent struct {
	Message string
}

// reporter tells the UI what the backend does.
type reporter struct{}

func (reporter) printf(format string, args ...any) {
	bus.Publish(&ReportEvent{Message: fmt.Sprintf(format, args...)})
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
