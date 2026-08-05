// Command taugrid-portal serves the Tau experiment and observability web
// surface: the Stellar experiment store and dashboards, and the unified portal.
//
// It was split out of the tau CLI so that submitting a workload does not link
// an embedded web frontend, an experiment database, and a Kusto query stack.
// The verb surface is unchanged: `taugrid-portal experiment ...` and
// `taugrid-portal portal ...` behave exactly as `tau experiment ...` and
// `tau portal ...` did.
package main

import (
	"fmt"
	"os"

	"github.com/Azure/taugrid/portal/internal/cli"
)

func main() {
	if err := cli.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
