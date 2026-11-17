// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// MetaTargetingQuery is a Meta targeting taxonomy search (#550): the kind (interest, behavior,
// work_position) and the text to match against option names.
type MetaTargetingQuery struct {
	Kind  string
	Query string
}

// MetaTargetingOption is one option a Meta targeting search matched. ID is exactly what the
// matching metaConfig.targeting list (interests, behaviors, workPositions) takes. The size fields
// are Meta's estimates, nil when Meta reported none.
type MetaTargetingOption struct {
	ID            string
	Name          string
	Path          []string
	Description   string
	AudienceLower *int64
	AudienceUpper *int64
	Coverage      *int64
}
