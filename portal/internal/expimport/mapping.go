// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expimport

import "github.com/Azure/taugrid/core/exptelemetry"

func IsStandardResearchMetric(name string) bool {
	return exptelemetry.IsStandardResearchMetric(name)
}

func ResearchMetricCard(name string) string {
	return exptelemetry.ResearchMetricCard(name)
}
