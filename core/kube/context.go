// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import "os"

const ContextEnv = "TAU_CONTEXT"

func DefaultContext() string {
	return os.Getenv(ContextEnv)
}

func ContextHelp() string {
	return "kubectl context (default: $" + ContextEnv + " or current)"
}
