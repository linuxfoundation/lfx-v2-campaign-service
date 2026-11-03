// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// ConversionAction is one conversion action on an ad account. ID and ResourceName are the two
// forms the platform's campaign config accepts as a conversion action.
type ConversionAction struct {
	ID             string
	ResourceName   string
	Name           string
	Status         string
	Type           string
	Category       string
	PrimaryForGoal bool
}

// ConversionActions is a conversion-action read: the account it was read from, and its actions.
type ConversionActions struct {
	AccountID string
	Actions   []ConversionAction
}
