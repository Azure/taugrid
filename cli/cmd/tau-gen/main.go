// Command tau-gen generates Tau-ready research workspace repositories.
package main

import (
	"fmt"
	"os"

	"github.com/Azure/taugrid/cli/internal/cli"
)

func main() {
	if err := cli.NewRepoGenRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
