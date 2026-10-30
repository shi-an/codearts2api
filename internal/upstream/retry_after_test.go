package upstream

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		input string
		want  time.Duration
	}{
		{"60", time.Minute},
		{now.Add(time.Minute).Format(http.TimeFormat), time.Minute},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"", 0}, {"invalid", 0}, {"-1", 0},
	} {
		if got := parseRetryAfter(tc.input, now); got != tc.want {
			t.Errorf("parseRetryAfter(%q)=%s, want %s", tc.input, got, tc.want)
		}
	}
}
