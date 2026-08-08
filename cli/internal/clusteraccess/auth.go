// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package clusteraccess obtains and normalizes normal-user AKS access for Tau.
package clusteraccess

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity/cache"
)

const (
	AuthModeInteractive = "interactive"
	AuthModeDeviceCode  = "devicecode"
)

type CredentialFactory interface {
	Credential(context.Context, string) (azcore.TokenCredential, error)
}

type UserCredentialFactory struct {
	Mode   string
	Output io.Writer
}

func (f UserCredentialFactory) Credential(ctx context.Context, tenantID string) (azcore.TokenCredential, error) {
	mode := strings.ToLower(strings.TrimSpace(f.Mode))
	if mode == "" {
		mode = AuthModeInteractive
	}
	var tokenCache azidentity.Cache
	if persistentCache, err := cache.New(&cache.Options{Name: "tau-aks-user"}); err == nil {
		tokenCache = persistentCache
	}
	if _, err := exec.LookPath("az"); err == nil {
		if credential, credentialErr := azidentity.NewAzureCLICredential(&azidentity.AzureCLICredentialOptions{
			TenantID: tenantID,
		}); credentialErr == nil {
			if _, tokenErr := credential.GetToken(ctx, policy.TokenRequestOptions{
				Scopes: []string{"https://management.azure.com/.default"},
			}); tokenErr == nil {
				return credential, nil
			}
		}
	}
	switch mode {
	case AuthModeInteractive:
		credential, err := azidentity.NewInteractiveBrowserCredential(&azidentity.InteractiveBrowserCredentialOptions{
			TenantID: tenantID,
			Cache:    tokenCache,
		})
		if err != nil {
			return nil, fmt.Errorf("create interactive Azure credential: %w", err)
		}
		return credential, nil
	case AuthModeDeviceCode:
		output := f.Output
		if output == nil {
			output = os.Stderr
		}
		credential, err := azidentity.NewDeviceCodeCredential(&azidentity.DeviceCodeCredentialOptions{
			TenantID: tenantID,
			Cache:    tokenCache,
			UserPrompt: func(_ context.Context, message azidentity.DeviceCodeMessage) error {
				_, err := fmt.Fprintln(output, message.Message)
				return err
			},
		})
		if err != nil {
			return nil, fmt.Errorf("create Azure device-code credential: %w", err)
		}
		return credential, nil
	default:
		return nil, fmt.Errorf("unsupported Tau Azure auth mode %q; use interactive or devicecode", f.Mode)
	}
}
