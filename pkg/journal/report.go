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

// kilobytes keeps the size precise enough to see a save change it.
func kilobytes(n int) string {
	kb := (n + 512) / 1024
	digits := fmt.Sprint(kb)
	var out []byte
	for i := range len(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, digits[i])
	}
	return string(out) + " KB"
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
