// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// +kubebuilder:object:generate=true
// +groupName=tau.azure.com
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	GroupName = "tau.azure.com"
	Version   = "v1alpha1"
)

var (
	GroupVersion  = schema.GroupVersion{Group: GroupName, Version: Version}
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)
