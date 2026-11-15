package ui

import (
	"strings"

	"github.com/projectbluefin/bluefin-report/internal/snapshot"
)

// familyName returns the display name of a metric row: the family name, or the
// metric name plus its object when the metric is tied to a specific object.
// familyFor reverses it to find the metric's family.
func familyName(m snapshot.Metric) string {
	name := m.Name
	if m.Object != "" {
		name += " [" + m.Object + "]"
	}
	return name
}

// familyFor looks up a metric's family descriptions. It reverses familyName:
// it strips the "user: " prefix the list prepends and the " [object]" suffix it
// appends, so the lookup matches the family names describe reports by name only.
func familyFor(name string, fams map[string]snapshot.Family) snapshot.Family {
	if strings.HasPrefix(name, "user: ") {
		name = strings.TrimPrefix(name, "user: ")
	}
	if i := strings.Index(name, " ["); i >= 0 { // rows append " [object]"; families are keyed by name only
		name = name[:i]
	}
	return fams[name]
}

// metricTooltip builds the hover text for a metric row. A known family with a
// plain-words description shows the metric name and value, then its family
// description and type; an unknown or undescribed family falls back to
// "name = value".
func metricTooltip(name string, val string, fams map[string]snapshot.Family) string {
	tip := name + " = " + val
	if fam := familyFor(name, fams); fam.Description != "" {
		tip += "\n" + fam.Description
		if fam.Type != "" {
			tip += " · " + fam.Type
		}
	}
	return tip
}
