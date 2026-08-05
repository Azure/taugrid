// Command tau is the Kubernetes-native AI runtime CLI.
//
// The public documentation defines the command and behavior contracts. The
// researcher-facing command groups are cluster, workspace, run, serve, data,
// python, and version; see internal/cli.NewRoot for the wiring.
package main

import (
	"fmt"
	"os"

	"github.com/Azure/taugrid/cli/internal/cli"
)

func main() {
	if err := cli.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
