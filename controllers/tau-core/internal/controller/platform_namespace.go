// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"

func platformNamespace(namespace string) string {
	if namespace != "" {
		return namespace
	}
	return tauv1alpha1.PlatformNamespace
}
