package ui

import (
	"testing"

	"github.com/projectbluefin/bluefin-report/internal/snapshot"
)

// "Include User Session" must not offer to attach an empty user report (#85).
func TestUserSessionRow(t *testing.T) {
	for _, c := range []struct {
		name    string
		user    snapshot.Report
		enabled bool
		want    string
	}{
		{"empty user report", snapshot.Report{}, false, "No user-session data was collected"},
		{"empty metrics array", snapshot.Report{Metrics: []snapshot.Metric{}}, false, "No user-session data was collected"},
		{"one metric", snapshot.Report{Metrics: make([]snapshot.Metric, 1)}, true,
			"Adds 1 entry about your desktop session’s services"},
		{"many metrics", snapshot.Report{Metrics: make([]snapshot.Metric, 42)}, true,
			"Adds 42 entries about your desktop session’s services"},
	} {
		t.Run(c.name, func(t *testing.T) {
			enabled, got := userSessionRow(c.user)
			if enabled != c.enabled || got != c.want {
				t.Errorf("userSessionRow = (%v, %q), want (%v, %q)", enabled, got, c.enabled, c.want)
			}
		})
	}
}
