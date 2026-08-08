// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import "os"

// ContextEnv is the environment variable Tau reads when no --context flag is
// given. It is spelled once here because the tau CLI and the taugrid-portal
// binary both expose --context flags and must agree on the fallback.
//
// Exported because it is also the answer to "did the caller name a cluster?" —
// a question the connection layer has to ask, and got wrong for as long as it
// asked cobra whether the flag was typed instead.
const ContextEnv = "TAU_CONTEXT"

// DefaultContext returns the kubectl context Tau uses when a command's
// --context flag is left empty. An empty result means "whatever kubectl's
// current context is".
func DefaultContext() string {
	return os.Getenv(ContextEnv)
}

// ContextHelp is the flag help text for --context on commands that work
// against whichever cluster kubectl is pointed at.
func ContextHelp() string {
	return "kubectl context (default: $" + ContextEnv + " or current)"
}
