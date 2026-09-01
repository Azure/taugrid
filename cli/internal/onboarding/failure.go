// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package onboarding

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

type FailureOwner string

const (
	FailureOwnerResearcher FailureOwner = "Researcher action required"
	FailureOwnerPlatform   FailureOwner = "Platform action required"
	FailureOwnerTransient  FailureOwner = "Transient infrastructure condition"
)

type Guidance struct {
	Owner  FailureOwner
	Action string
}

type GuidedError struct {
	Cause    error
	Guidance Guidance
}

func (e *GuidedError) Error() string {
	return fmt.Sprintf("%v\n\nOwner: %s\nAction: %s", e.Cause, e.Guidance.Owner, e.Guidance.Action)
}

func (e *GuidedError) Unwrap() error {
	return e.Cause
}

func Explain(err error) error {
	if err == nil {
		return nil
	}
	var already *GuidedError
	if errors.As(err, &already) {
		return err
	}
	return &GuidedError{Cause: err, Guidance: Classify(err)}
}

func Classify(err error) Guidance {
	switch {
	case errors.Is(err, workspaceconnection.ErrDescriptorNotFound):
		return Guidance{
			Owner:  FailureOwnerResearcher,
			Action: "Run from a Tau-enabled repository containing tau/workspace.connection.yaml, or ask the project owner to add it.",
		}
	case errors.Is(err, workspaceconnection.ErrInteractiveRequired):
		return Guidance{
			Owner:  FailureOwnerResearcher,
			Action: "Run `tau workspace connection` in an interactive terminal. Review the destination and approve it only if it matches your platform handoff, then retry your original command.",
		}
	case errors.Is(err, workspaceconnection.ErrConnectionDeclined):
		return Guidance{
			Owner:  FailureOwnerResearcher,
			Action: "No connection was saved. Run `tau workspace connection` again when ready, and approve it only if the destination is expected.",
		}
	}
	var responseError *azcore.ResponseError
	if errors.As(err, &responseError) {
		switch responseError.StatusCode {
		case 401:
			return Guidance{
				Owner:  FailureOwnerResearcher,
				Action: "Sign in with the Entra account named in your onboarding instructions and retry.",
			}
		case 403:
			return Guidance{
				Owner:  FailureOwnerPlatform,
				Action: "Ask the platform owner to grant Azure Kubernetes Service Cluster User access for the descriptor's AKS resource.",
			}
		}
	}
	var (
		operationError *net.OpError
		dnsError       *net.DNSError
		addressError   *net.AddrError
	)
	if errors.As(err, &operationError) ||
		errors.As(err, &dnsError) ||
		errors.As(err, &addressError) {
		return Guidance{
			Owner:  FailureOwnerResearcher,
			Action: "Check network and cluster DNS reachability, then retry the same run.",
		}
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "no such host"),
		strings.Contains(message, "connection refused"),
		strings.Contains(message, "i/o timeout"),
		strings.Contains(message, "tls handshake timeout"):
		return Guidance{
			Owner:  FailureOwnerResearcher,
			Action: "Check network and cluster DNS reachability, then retry the same run.",
		}
	case strings.Contains(message, "not ready"),
		strings.Contains(message, "missing required permission"),
		strings.Contains(message, "localqueue"),
		strings.Contains(message, "read tauworkspace"),
		strings.Contains(message, "not entra exec authentication"),
		strings.Contains(message, "static credentials"):
		return Guidance{
			Owner:  FailureOwnerPlatform,
			Action: "Send this error to the workspace owner; AKS Entra authentication, workspace readiness, RBAC, and queue policy are platform-managed.",
		}
	case strings.Contains(message, "field is immutable"):
		return Guidance{
			Owner:  FailureOwnerResearcher,
			Action: "Delete the existing workload with `tau run cancel <name>` and re-submit.",
		}
	case strings.Contains(message, "imagepullbackoff"),
		strings.Contains(message, "errimagepull"):
		return Guidance{
			Owner:  FailureOwnerPlatform,
			Action: "Ask the project or platform owner to verify the workload image is pullable from this cluster.",
		}
	case strings.Contains(message, "deadline exceeded"),
		strings.Contains(message, "timed out"):
		return Guidance{
			Owner:  FailureOwnerTransient,
			Action: "The run ID is preserved. Check `tau run status <run-id>` for queue pressure and retry only if the condition clears.",
		}
	default:
		return Guidance{
			Owner:  FailureOwnerResearcher,
			Action: "Correct the reported repository or workload input and rerun the same command.",
		}
	}
}
