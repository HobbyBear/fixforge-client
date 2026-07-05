//go:build !windows

package runner

import "testing"

func TestSystemdPathValueDoesNotQuoteAbsolutePath(t *testing.T) {
	got := systemdPathValue("/home/xch")
	if got != "/home/xch" {
		t.Fatalf("systemdPathValue() = %q, want %q", got, "/home/xch")
	}
}

func TestSystemdPathValueEscapesSpacesAndSpecifiers(t *testing.T) {
	got := systemdPathValue("/home/xch/FixForge Runner 100%")
	want := `/home/xch/FixForge\x20Runner\x20100%%`
	if got != want {
		t.Fatalf("systemdPathValue() = %q, want %q", got, want)
	}
}

func TestSystemdEnvironmentAssignmentQuotesWholeAssignment(t *testing.T) {
	got := systemdEnvironmentAssignment("HOME", "/home/xch/FixForge Runner")
	want := `"` + `HOME=/home/xch/FixForge Runner` + `"`
	if got != want {
		t.Fatalf("systemdEnvironmentAssignment() = %q, want %q", got, want)
	}
}
