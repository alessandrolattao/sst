package ui

import (
	"testing"
	"time"
)

func TestWithDuration(t *testing.T) {
	cases := []struct {
		duration time.Duration
		want     string
	}{
		{0, "services/users/get"},
		{800 * time.Millisecond, "services/users/get"},
		{time.Second, "services/users/get"},
		{1500 * time.Millisecond, "services/users/get (1.5s)"},
		{12340 * time.Millisecond, "services/users/get (12.3s)"},
	}
	for _, c := range cases {
		if got := withDuration("services/users/get", c.duration); got != c.want {
			t.Errorf("withDuration(%v) = %q, want %q", c.duration, got, c.want)
		}
	}
}
