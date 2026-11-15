package ui

import (
	"strings"
	"testing"

	"github.com/projectbluefin/bluefin-report/internal/snapshot"
)

func TestFamilyName(t *testing.T) {
	cases := []struct {
		name string
		m    snapshot.Metric
		want string
	}{
		{name: "plain", m: snapshot.Metric{Name: "io.systemd.Basic.OSRelease.NAME"}, want: "io.systemd.Basic.OSRelease.NAME"},
		{name: "object", m: snapshot.Metric{Name: "io.systemd.DBus.Introspect.Object", Object: "org.freedesktop.systemd1"}, want: "io.systemd.DBus.Introspect.Object [org.freedesktop.systemd1]"},
		{name: "solo", m: snapshot.Metric{Name: "solo"}, want: "solo"},
	}
	for _, c := range cases {
		if got := familyName(c.m); got != c.want {
			t.Errorf("familyName(%q): got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFamilyFor(t *testing.T) {
	fams := map[string]snapshot.Family{
		"io.systemd.Basic.OSRelease.NAME": {Description: "The operating system name", Type: "Basic"},
	}
	// The metrics list prefixes user metrics with "user: "; strip it before lookup.
	if fam := familyFor("user: io.systemd.Basic.OSRelease.NAME", fams); fam.Description != "The operating system name" {
		t.Errorf("familyFor(user:): got %+v, want the OSRelease family", fam)
	}
	// A metric with no family at all returns the zero Family.
	if fam := familyFor("io.unknown.metric", fams); fam.Description != "" || fam.Type != "" {
		t.Errorf("familyFor(unknown): got %+v, want zero Family", fam)
	}
}

func TestMetricTooltip(t *testing.T) {
	fams := map[string]snapshot.Family{
		"io.systemd.Basic.OSRelease.NAME": {Description: "The operating system name", Type: "Basic"},
	}
	// Known family: name = value, then description and type.
	if got := metricTooltip("io.systemd.Basic.OSRelease.NAME", "Bluefin", fams); got != "io.systemd.Basic.OSRelease.NAME = Bluefin\nThe operating system name · Basic" {
		t.Errorf("metricTooltip known: got %q", got)
	}
	// Unknown family falls back to name = value.
	if got := metricTooltip("io.unknown.metric", "x", fams); got != "io.unknown.metric = x" {
		t.Errorf("metricTooltip unknown: got %q, want fallback", got)
	}
	// A family with no description falls back to name = value.
	if got := metricTooltip("io.systemd.Basic.OSRelease.NAME", "Bluefin", map[string]snapshot.Family{
		"io.systemd.Basic.OSRelease.NAME": {Description: ""},
	}); got != "io.systemd.Basic.OSRelease.NAME = Bluefin" {
		t.Errorf("metricTooltip empty-desc: got %q, want fallback", got)
	}
}

func TestMetricTooltipStripsObject(t *testing.T) {
	fams := map[string]snapshot.Family{
		"io.systemd.Manager.UnitActiveState": {Description: "State of a systemd unit", Type: "Manager"},
	}
	// A metric tied to an object is rendered as "name [object]"; the tooltip must
	// still resolve the family behind it, not fall back to name = value.
	got := metricTooltip("io.systemd.Manager.UnitActiveState [sshd.service]", "active", fams)
	want := "io.systemd.Manager.UnitActiveState [sshd.service] = active\nState of a systemd unit · Manager"
	if got != want {
		t.Errorf("metricTooltip object: got %q, want %q", got, want)
	}
}

func TestMetricTooltipStripsUserPrefix(t *testing.T) {
	fams := map[string]snapshot.Family{
		"io.systemd.Basic.OSRelease.NAME": {Description: "The operating system name", Type: "Basic"},
	}
	// The row name carries the "user: " prefix; the tooltip must still resolve it.
	got := metricTooltip("user: io.systemd.Basic.OSRelease.NAME", "Bluefin", fams)
	if !strings.Contains(got, "The operating system name") {
		t.Errorf("metricTooltip user: prefix not resolved, got %q", got)
	}
}

func TestMetricTooltipFallsBackWithoutDescription(t *testing.T) {
	// The tooltip pulls family description and type from system-report describe.
	// Per the design, an empty description falls back to name = value even when a
	// Type is present, so the tooltip never shows a bare "· Type" line.
	fams := map[string]snapshot.Family{
		"io.systemd.DBus.Introspect.Object": {Type: "DBus"},
	}
	got := metricTooltip("io.systemd.DBus.Introspect.Object", "true", fams)
	want := "io.systemd.DBus.Introspect.Object = true"
	if got != want {
		t.Errorf("metricTooltip type-only: got %q, want %q", got, want)
	}
}
