// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

// Helpers shared by more than one test file in this package.
//
// strPtr lived in orchestrator_test.go, which was fine while that was its only caller.
// connection_test.go and email_copy_test.go now use it too, and Go compiling every _test.go
// in a package together makes that dependency invisible: nothing in either file says where
// the symbol comes from, and splitting or renaming orchestrator_test.go would break two
// unrelated files' build for no reason a reader could see from them. A file whose name says
// "shared" is the declaration of that fact.
//
// Keep this file for helpers with more than one caller. A helper used by exactly one test
// file belongs in that file, where its context is.

// strPtr returns a pointer to s, for the optional string fields that Goa generates as
// *string. Test tables need an addressable value and a literal is not one.
func strPtr(s string) *string { return &s }
