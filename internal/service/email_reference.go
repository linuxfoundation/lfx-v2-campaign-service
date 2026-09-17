// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// referenceCandidateLimit is how many of a project's most recently updated PUBLISHED marketing
// emails EmailReferenceSource considers. Three, matching the reference-implementation prototype
// this ports (see the package doc comment below): enough to find a genuinely rich primary
// template and a couple of others for the style corpus, without the O(n) body fetch this makes
// (one GetEmailHTMLWidgets call per candidate) growing unbounded on a busy portal.
const referenceCandidateLimit = 3

// referencePrimaryExcerptRunes bounds the primary template's plain-text excerpt.
// referenceStyleCorpusRunes bounds the combined excerpt from the OTHER candidates (style/tone
// only, not a structural template). Together with label text these stay well under
// maxReferenceBlockRunes, which BuildReferenceBlock enforces as a hard backstop regardless.
const (
	referencePrimaryExcerptRunes = 1200
	referenceStyleCorpusRunes    = 500
)

// hubspotCreds is the credential shape stored (encrypted) for a HubSpot connection. Mirrors
// internal/dispatch's hubspotCreds byte-for-byte (same persisted JSON shape, no json tag), kept
// as its own copy rather than imported: internal/dispatch already has three in-package test files
// that import internal/service (status_toggler_guard_test.go, accountlister_prose_parity_test.go,
// account_discovery_test.go), so internal/service importing internal/dispatch would create a Go
// import cycle when dispatch's own test binary is built.
type hubspotCreds struct {
	PrivateAppToken string
}

// EmailReferenceSource is a best-effort, read-only lookup of a project's own past sent HubSpot
// marketing emails, used to build a style/tone reference block for GenerateEmailCopy.
//
// Ports the reference implementation's approach (an internal prototype's content generator: read
// up to a handful of past sent emails, pick the richest as the primary structural template, build
// a short style corpus from the rest, inject both into the prompt) without pulling in
// internal/dispatch's credential-resolution engine (credsSource in creds.go). That machinery
// exists to correctly attribute PAID-AD campaign failures to the right account across a dense set
// of provenance-matching rules — overkill for a read-only, best-effort enrichment that silently
// yields nothing on any error, and importing it would create the cycle hubspotCreds' doc comment
// describes. This type instead does the minimal project-then-system connection lookup itself.
type EmailReferenceSource struct {
	conn domain.ConnectionReader
	enc  domain.Encryptor
	opts []hubspot.Option
}

// NewEmailReferenceSource builds a reference source. opts are forwarded to hubspot.NewClient
// (e.g. test overrides for the base URL or HTTP client).
func NewEmailReferenceSource(conn domain.ConnectionReader, enc domain.Encryptor, opts ...hubspot.Option) *EmailReferenceSource {
	return &EmailReferenceSource{conn: conn, enc: enc, opts: opts}
}

// BuildReferenceBlock returns a truncated, plain-text style/tone reference block built from the
// project's own most recently published HubSpot marketing emails, or "" if none could be found.
//
// BEST-EFFORT: every failure (no connection, inactive connection, malformed credentials, a
// HubSpot API error, zero published emails) is logged at most and yields "", never an error —
// this enriches email-copy generation, it does not gate it. Callers must not treat "" as
// distinguishable from "lookup failed"; GenerateEmailCopy does not need to.
func (r *EmailReferenceSource) BuildReferenceBlock(ctx context.Context, projectID string) string {
	client, err := r.resolveClient(ctx, projectID)
	if err != nil {
		slog.DebugContext(ctx, "email reference lookup skipped: could not resolve a hubspot client",
			"project_id", projectID, "error", err)
		return ""
	}

	emails, err := client.SearchEmails(ctx, "")
	if err != nil {
		slog.WarnContext(ctx, "email reference lookup skipped: hubspot search failed",
			"project_id", projectID, "error", err)
		return ""
	}

	candidates := publishedEmails(emails)
	if len(candidates) == 0 {
		return ""
	}
	if len(candidates) > referenceCandidateLimit {
		candidates = candidates[:referenceCandidateLimit]
	}

	type excerpt struct {
		subject string
		text    string
	}
	excerpts := make([]excerpt, 0, len(candidates))
	for _, e := range candidates {
		blocks, berr := client.GetEmailHTMLWidgets(ctx, e.ID)
		if berr != nil {
			slog.DebugContext(ctx, "email reference lookup: skipping one candidate",
				"project_id", projectID, "email_id", e.ID, "error", berr)
			continue
		}
		text := plainTextFromBlocks(blocks)
		if text == "" {
			continue
		}
		excerpts = append(excerpts, excerpt{subject: e.Subject, text: text})
	}
	if len(excerpts) == 0 {
		return ""
	}

	// The RICHEST candidate (most extracted plain text) becomes the primary structural template.
	// Ties keep the first (most recently updated, since SearchEmails already sorted candidates
	// that way), matching the "most recent wins" default a picker would apply.
	primary := 0
	for i := 1; i < len(excerpts); i++ {
		if len(excerpts[i].text) > len(excerpts[primary].text) {
			primary = i
		}
	}

	var b strings.Builder
	b.WriteString("Primary structural template (subject: \"")
	b.WriteString(excerpts[primary].subject)
	b.WriteString("\"):\n")
	b.WriteString(truncateRunes(excerpts[primary].text, referencePrimaryExcerptRunes))

	var corpus []string
	for i, e := range excerpts {
		if i == primary {
			continue
		}
		corpus = append(corpus, e.text)
	}
	if len(corpus) > 0 {
		b.WriteString("\n\nAdditional style/tone samples:\n")
		b.WriteString(truncateRunes(strings.Join(corpus, "\n---\n"), referenceStyleCorpusRunes))
	}

	return truncateRunes(b.String(), maxReferenceBlockRunes)
}

// disconnectedReader is the optional half of the connection port: whether a project
// explicitly disconnected a provider, as opposed to never having connected it.
//
// It is a separate, optionally-satisfied interface rather than a widening of
// domain.ConnectionReader because every other reader (and every test fake) would otherwise
// have to grow a method only this fallback needs. The postgres ConnectionRepo already
// implements it.
type disconnectedReader interface {
	Disconnected(ctx context.Context, projectID string, provider model.Provider) (bool, error)
}

// disconnected reports whether projectID explicitly disconnected HubSpot.
//
// A reader that cannot answer returns false: without the probe there is no evidence of a
// disconnect, and refusing every fallback on that basis would break the shared-portal case
// this source exists to serve. That is a deliberate narrowing of the fail-closed rule to
// "cannot ask" — an error from a reader that CAN ask still fails closed at the call site.
func (r *EmailReferenceSource) disconnected(ctx context.Context, projectID string) (bool, error) {
	probe, ok := r.conn.(disconnectedReader)
	if !ok {
		return false, nil
	}
	return probe.Disconnected(ctx, projectID, model.ProviderHubSpot)
}

// resolveClient builds a HubSpot client from the project's own connection, falling back to the
// LF system-wide connection when the project has none — the same fallback shape
// internal/dispatch's credsSource.resolve uses for every other HubSpot call, since ALL LF
// foundations share one HubSpot portal absent a project-specific override.
func (r *EmailReferenceSource) resolveClient(ctx context.Context, projectID string) (*hubspot.Client, error) {
	conn, err := r.conn.Get(ctx, projectID, model.ProviderHubSpot)
	if errors.Is(err, domain.ErrNotFound) {
		// ErrNotFound does NOT mean "this project never connected". Connections are
		// soft-deleted and Get filters `status <> 'deleted'`, so a project that explicitly
		// DISCONNECTED HubSpot is indistinguishable here from one that never connected — and
		// falling back for the first would style that project's copy from the LF portal it
		// just opted out of. The probe is what separates them.
		//
		// Fails CLOSED on a probe error, matching dispatch's systemConn: an unanswered "was
		// this disconnected?" is not a no.
		if disconnected, derr := r.disconnected(ctx, projectID); derr != nil {
			return nil, fmt.Errorf("could not determine whether %s disconnected hubspot: %w", projectID, derr)
		} else if disconnected {
			return nil, domain.ErrNotFound
		}
		conn, err = r.conn.Get(ctx, model.SystemProjectID, model.ProviderHubSpot)
	}
	if err != nil {
		return nil, err
	}
	// domain.ConnectionReader does not forbid a (nil, nil) return, and dereferencing that
	// would panic rather than degrade — the one failure mode this best-effort caller cannot
	// absorb, since it takes the whole request down instead of dropping the reference block.
	if conn == nil {
		return nil, domain.ErrNotFound
	}
	if conn.Status != model.StatusActive || !conn.HasCredentials() {
		return nil, errors.New("hubspot connection is not usable")
	}

	plaintext, err := r.enc.Decrypt(conn.EncryptedCredentials)
	if err != nil {
		return nil, err
	}
	var creds hubspotCreds
	if err := json.Unmarshal(plaintext, &creds); err != nil {
		return nil, err
	}
	token := strings.TrimSpace(creds.PrivateAppToken)
	if token == "" {
		return nil, errors.New("hubspot credentials are incomplete")
	}

	return hubspot.NewClient(
		hubspot.Credentials{PrivateAppToken: token},
		hubspot.AccountConfig{PortalID: conn.ProviderConfig["portal_id"]},
		r.opts...,
	), nil
}

// publishedEmails filters to PUBLISHED (sent) emails, dropping drafts and A/B variant drafts —
// neither is a real sent email a reference should mirror. Order is preserved: SearchEmails
// already returns most-recently-updated first (sortEmailsByUpdatedDesc), and filtering a slice
// cannot reorder what it keeps.
func publishedEmails(emails []hubspot.Email) []hubspot.Email {
	out := make([]hubspot.Email, 0, len(emails))
	for _, e := range emails {
		if strings.EqualFold(strings.TrimSpace(e.State), "PUBLISHED") {
			out = append(out, e)
		}
	}
	return out
}

// htmlTagPattern strips markup for a plain-text excerpt. A reference block feeds an LLM prompt,
// not a renderer, so approximate stripping (no entity decoding beyond the handful HubSpot's own
// editor commonly emits) is sufficient — the goal is tone and structure, not a faithful render.
var htmlTagPattern = regexp.MustCompile(`<[^>]*>`)

var htmlEntityReplacer = strings.NewReplacer(
	"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&#39;", "'", "&quot;", `"`,
)

// plainTextFromBlocks concatenates an email draft's PLACED rich-text blocks (reading order; see
// hubspot.EmailHTMLBlock.Placed) into one plain-text string, collapsing runs of whitespace left
// behind by stripped tags.
func plainTextFromBlocks(blocks []hubspot.EmailHTMLBlock) string {
	var parts []string
	for _, blk := range blocks {
		if !blk.Placed || strings.TrimSpace(blk.HTML) == "" {
			continue
		}
		text := htmlEntityReplacer.Replace(htmlTagPattern.ReplaceAllString(blk.HTML, " "))
		text = strings.Join(strings.Fields(text), " ")
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// truncateRunes cuts s to at most n runes, appending no ellipsis: this feeds a prompt block, and
// an abrupt cut costs nothing a model would otherwise use as a structural cue.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
