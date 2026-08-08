// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
)

type stubPVCRunner struct {
	out string
	err error
}

func (s stubPVCRunner) Raw(_ context.Context, _ []string, _ []byte) (string, error) {
	return s.out, s.err
}

func workspaceWithNamespace(ns string) tauworkspace.Workspace {
	var w tauworkspace.Workspace
	w.Status.Target.ResolvedNamespace = ns
	return w
}

// A conclusive NotFound is the one case where the claim's absence is known, so
// it stays a warning: storage is opt-in and CPU configs never mount /data.
func TestReportWorkspaceDataPVCNotFoundWarnsWithoutFailing(t *testing.T) {
	var out bytes.Buffer
	err := reportWorkspaceDataPVC(
		context.Background(),
		stubPVCRunner{err: errors.New(`Error from server (NotFound): persistentvolumeclaims "blob-training" not found`)},
		&out,
		workspaceWithNamespace("team-a"),
		"blob-training",
	)
	if err != nil {
		t.Fatalf("expected no error for conclusive NotFound, got %v", err)
	}
	if !strings.Contains(out.String(), "not found in namespace \"team-a\"") {
		t.Fatalf("expected missing-PVC warning, got %q", out.String())
	}
}

// Forbidden and transport failures leave the claim's existence unknown. The
// check must not report them as optional missing storage.
func TestReportWorkspaceDataPVCInconclusiveLookupFails(t *testing.T) {
	for name, lookupErr := range map[string]error{
		"forbidden": errors.New(`Error from server (Forbidden): persistentvolumeclaims is forbidden`),
		"timeout":   errors.New("Unable to connect to the server: dial tcp: i/o timeout"),
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := reportWorkspaceDataPVC(
				context.Background(),
				stubPVCRunner{err: lookupErr},
				&out,
				workspaceWithNamespace("team-a"),
				"blob-training",
			)
			if err == nil {
				t.Fatal("expected an error when the PVC lookup was inconclusive")
			}
			if strings.Contains(out.String(), "not found") {
				t.Fatalf("inconclusive lookup must not be reported as missing storage, got %q", out.String())
			}
		})
	}
}

func TestReportWorkspaceDataPVCMalformedJSONFails(t *testing.T) {
	var out bytes.Buffer
	err := reportWorkspaceDataPVC(
		context.Background(),
		stubPVCRunner{out: "not json"},
		&out,
		workspaceWithNamespace("team-a"),
		"blob-training",
	)
	if err == nil {
		t.Fatal("expected an error when kubectl output could not be parsed")
	}
}

func TestReportWorkspaceDataPVCBoundIsSilent(t *testing.T) {
	var out bytes.Buffer
	err := reportWorkspaceDataPVC(
		context.Background(),
		stubPVCRunner{out: `{"status":{"phase":"Bound"}}`},
		&out,
		workspaceWithNamespace("team-a"),
		"blob-training",
	)
	if err != nil {
		t.Fatalf("expected no error for a Bound claim, got %v", err)
	}
	if out.String() != "" {
		t.Fatalf("expected no output for a Bound claim, got %q", out.String())
	}
}

func TestReportWorkspaceDataPVCUnboundWarns(t *testing.T) {
	var out bytes.Buffer
	err := reportWorkspaceDataPVC(
		context.Background(),
		stubPVCRunner{out: `{"status":{"phase":"Pending"}}`},
		&out,
		workspaceWithNamespace("team-a"),
		"blob-training",
	)
	if err != nil {
		t.Fatalf("expected no error for an unbound claim, got %v", err)
	}
	if !strings.Contains(out.String(), "is Pending, not Bound") {
		t.Fatalf("expected unbound warning, got %q", out.String())
	}
}
