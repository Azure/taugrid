// tau-demo stands in for the pieces of the TauGrid 278 success path a laptop
// cluster can exercise: the tau CLI (submit, status) and a fake AKS Cluster
// Autoscaler that answers ProvisioningRequests.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const usage = `tau-demo <command> [flags]

  submit    render a suspended Job like 'tau run' (with capacity preflight) and apply it
  status    show startup phases for a run, like 'tau run status' (add --watch)
  fake-ca   simulated AKS Cluster Autoscaler (atomic ProvisioningRequest scale-up)
  overview  one-screen view of queues, workloads, ProvisioningRequests and pool nodes
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c, err := newClients()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "submit":
		err = runSubmit(ctx, c, args)
	case "status":
		err = runStatus(ctx, c, args)
	case "fake-ca":
		err = runFakeCA(ctx, c, args)
	case "overview":
		err = runOverview(ctx, c, args)
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
