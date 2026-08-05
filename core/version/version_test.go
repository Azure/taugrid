package version

import (
	"strings"
	"testing"
)

func TestInfoIsHeadedByCallerName(t *testing.T) {
	for _, name := range []string{"tau", "taugrid-portal"} {
		got := Info(name)
		want := name + " " + Version
		if line, _, _ := strings.Cut(got, "\n"); line != want {
			t.Errorf("Info(%q) first line = %q, want %q", name, line, want)
		}
	}
}

// The package is linked by more than one binary, so no product name may be
// baked into it. A hardcoded prefix here is what made `taugrid-portal version`
// print "tau". Checking for "tau" also covers "taugrid-portal".
func TestInfoCarriesNoBuiltInProductName(t *testing.T) {
	if got := Info("probe"); strings.Contains(got, "tau") {
		t.Errorf(`Info("probe") = %q, must not mention a product name`, got)
	}
}

func TestInfoReportsCommitAndDate(t *testing.T) {
	got := Info("probe")
	for _, want := range []string{"  commit: " + Commit, "  built:  " + Date} {
		if !strings.Contains(got, want) {
			t.Errorf("Info() = %q, missing %q", got, want)
		}
	}
}
