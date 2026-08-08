package cli

import "testing"

func TestRunDiagnoseRegistersOnlyRoutingAndOutputFlags(t *testing.T) {
	cmd := newRunDiagnoseCmd()
	for _, name := range []string{"namespace", "workspace", "context", "output"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("missing --%s", name)
		}
	}
	for _, name := range []string{"tail", "log-limit-bytes", "event-limit"} {
		if cmd.Flags().Lookup(name) != nil {
			t.Fatalf("unexpected public tuning flag --%s", name)
		}
	}
}
