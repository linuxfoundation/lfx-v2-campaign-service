// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"encoding/json"
	"time"
)

// WizardPhase is where a wizard session has got to. The values match the CHECK
// constraint in migration 000033 and the `wizardPhaseEnum` vocabulary in
// design/brief_wizard.go — all three are the same list and must be changed together.
type WizardPhase string

// The wizard's lifecycle. The order is meaningful: a session only ever moves forward
// through it, and each value names what the session HAS, not what it is about to do.
const (
	// WizardPhasePlanning is the initial phase. The session row exists; the plan may or
	// may not have run yet. A session can sit here indefinitely — a human opened the
	// wizard and walked away — which is why an old planning row is not garbage.
	WizardPhasePlanning WizardPhase = "planning"
	// WizardPhaseContent means at least one content variant has been generated and is
	// awaiting approval or editing. Re-generating leaves the session in this phase.
	WizardPhaseContent WizardPhase = "content"
	// WizardPhaseCloned means a HubSpot draft exists for this session. This is the first
	// phase with an effect OUTSIDE this service, so it is also the first one that cannot
	// be undone by discarding the session.
	WizardPhaseCloned WizardPhase = "cloned"
	// WizardPhaseComplete means the draft's send list is set and the wizard has nothing
	// left to do. It does NOT mean the email was sent — a human still presses send in
	// HubSpot.
	WizardPhaseComplete WizardPhase = "complete"
)

// PhaseOrDefault returns the session's phase, treating the empty string as planning.
//
// It exists for the same reason CampaignAudience.StatusOrDefault does: the zero value of
// a freshly constructed struct must mean the same thing as the column's DEFAULT, or a
// session built in Go and one read back from Postgres would disagree about a row that
// never changed.
func (s *WizardSession) PhaseOrDefault() WizardPhase {
	if s.Phase == "" {
		return WizardPhasePlanning
	}
	return s.Phase
}

// WizardChatTurn is one message in a session's persisted conversation.
//
// Role is the model-facing role string ("user" or "assistant"), kept as a plain string
// rather than a typed enum because its only consumer is prompt composition, which passes
// it through to the model verbatim.
type WizardChatTurn struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	At      time.Time `json:"at"`
}

// WizardSession is one run of the email-creation wizard over a brief — the durable
// counterpart of migration 000033's row.
//
// The four payload fields are json.RawMessage rather than typed structs, and that is a
// decision rather than laziness. They hold model output and caller-supplied content
// blocks whose shape is set by the frontend contract (see design/brief_wizard.go's
// wire-shape note), and this service never reads INTO them: it stores what was generated
// and hands it back on the next turn. Typing them would freeze that frontend's block
// vocabulary into this service's domain model, so that adding a block type the UI
// already understands would require a migration here.
type WizardSession struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	BriefID   string `json:"brief_id"`

	Phase WizardPhase `json:"phase"`

	// ProgressToken addresses this session's SSE stream. Empty means the caller never
	// asked for progress frames, which is a normal way to drive the wizard.
	ProgressToken string `json:"progress_token,omitempty"`

	// PlanResult is the planning turn's outcome, replayed to late subscribers of the
	// progress stream and returned by the session endpoint.
	PlanResult json.RawMessage `json:"plan_result,omitempty"`
	// ReferenceVariant and StageVariant are the two independently-generated content
	// variants. Either may be absent, and either may record its own failure — a failed
	// variant is stored, not dropped, so the UI can show why.
	ReferenceVariant json.RawMessage `json:"reference_variant,omitempty"`
	StageVariant     json.RawMessage `json:"stage_variant,omitempty"`
	// Sections is the content-block list as the user last left it, which is what
	// update-sections rebuilds HTML from and what clone writes into the draft.
	Sections json.RawMessage `json:"sections,omitempty"`

	// EmailID and DraftURL name the HubSpot draft this session created. They are a cache
	// of what HubSpot owns; the same artifact is also recorded on the brief's campaign.
	EmailID  string `json:"email_id,omitempty"`
	DraftURL string `json:"draft_url,omitempty"`

	// ChatHistory is the full conversation, persisted so a pod change between two turns
	// does not silently strip the model's context.
	ChatHistory json.RawMessage `json:"chat_history,omitempty"`

	Version   int64     `json:"version"`
	CreatedBy *Actor    `json:"created_by,omitempty"`
	UpdatedBy *Actor    `json:"updated_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Turns decodes ChatHistory into its turns.
//
// An absent or empty history is NOT an error: a session with no chat turns is the normal
// state for most of a wizard run, so the caller gets an empty slice. A history that is
// present and undecodable IS an error — that is corruption, and a chat turn composed
// against silently-dropped context would produce a plausible answer to the wrong
// question.
func (s *WizardSession) Turns() ([]WizardChatTurn, error) {
	if len(s.ChatHistory) == 0 {
		return nil, nil
	}
	var turns []WizardChatTurn
	if err := json.Unmarshal(s.ChatHistory, &turns); err != nil {
		return nil, err
	}
	return turns, nil
}

// SetTurns encodes turns back into ChatHistory. A nil or empty slice is encoded as `[]`
// rather than left nil, matching the column's DEFAULT so a cleared history and a
// never-written one are stored identically.
func (s *WizardSession) SetTurns(turns []WizardChatTurn) error {
	if len(turns) == 0 {
		s.ChatHistory = json.RawMessage("[]")
		return nil
	}
	raw, err := json.Marshal(turns)
	if err != nil {
		return err
	}
	s.ChatHistory = raw
	return nil
}
