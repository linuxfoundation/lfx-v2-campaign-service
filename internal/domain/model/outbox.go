// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "encoding/json"

// OutboxMessage is a pending Query Service index message.
//
// It carries the fully-marshalled Payload rather than the resource, so the relay never
// re-derives it: a message enqueued under one contract publishes exactly as it was written, and
// a later contract change cannot retroactively alter what a pending row means.
type OutboxMessage struct {
	ID         int64
	ObjectType string
	ObjectID   string
	Payload    json.RawMessage
	Attempts   int
}
