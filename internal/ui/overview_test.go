package ui

import "testing"

// copyFactTooltip names the copy button for a system-detail field. It must read
// as a plain instruction to someone who is not a developer, not a field code.
func TestCopyFactTooltip(t *testing.T) {
	for _, tc := range []struct{ title, want string }{
		{"Version", "Copy Version"},
		{"Kernel", "Copy Kernel"},
		{"Image ref", "Copy Image ref"},
	} {
		if got := copyFactTooltip(tc.title); got != tc.want {
			t.Errorf("copyFactTooltip(%q) = %q, want %q", tc.title, got, tc.want)
		}
	}
}
