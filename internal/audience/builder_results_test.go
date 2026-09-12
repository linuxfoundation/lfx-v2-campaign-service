// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import "testing"

// The suppression categories must match the enum the design publishes.
//
// The constant read "event" while design/audience_builder.go declares
// Enum("standard", "brand", "event_specific"), so every event-specific row fell outside the
// published contract — and a consumer grouping on the generated types dropped exactly the
// rows that outrank the other two. The design is a DSL and cannot reference these constants,
// so nothing but a test keeps the two in agreement.
func TestSuppressionCategoriesMatchThePublishedEnum(t *testing.T) {
	// Mirrors design/audience_builder.go: Enum("standard", "brand", "event_specific").
	published := map[string]bool{"standard": true, "brand": true, "event_specific": true}

	for _, category := range []string{SuppressionCategoryStandard, SuppressionCategoryBrand, SuppressionCategoryEvent} {
		if !published[category] {
			t.Errorf("category %q is not in the design's Enum(\"standard\", \"brand\", \"event_specific\"); "+
				"a consumer grouping on the generated contract will drop these rows", category)
		}
	}
}
