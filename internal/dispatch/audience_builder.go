// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/snowflake"
)

// PastEditionResolver is the Snowflake capability the builder needs. An interface (rather than
// *snowflake.Client) so the builder can be constructed without a warehouse — a deployment with
// no Snowflake config still builds country-only audiences instead of failing outright.
type PastEditionResolver interface {
	ResolvePastEventNames(ctx context.Context, eventTerm, locationTerm, currentYear string) ([]snowflake.Event, error)
}

// AudienceBuilder implements service.AudienceBuilder against the real platforms. It lives in
// this package because it needs the same per-project credential resolution the dispatchers use:
// HubSpot tokens are stored per project as encrypted connections, not injected as global config.
type AudienceBuilder struct {
	creds     *credsSource
	snowflake PastEditionResolver
	opts      []hubspot.Option
}

// NewAudienceBuilder builds the audience builder. snow may be nil: the warehouse is used only
// to widen an audience with past editions, so a nil resolver degrades to a country-only build
// rather than blocking the email channel entirely.
func NewAudienceBuilder(repo connReader, enc domain.Encryptor, snow PastEditionResolver, opts ...hubspot.Option) *AudienceBuilder {
	return &AudienceBuilder{creds: newCredsSource(repo, enc), snowflake: snow, opts: opts}
}

// ResolvePastEditions returns the VERBATIM names of an event's past editions.
//
// The names are used as exact HubSpot filter values, so they must come from the warehouse and
// never be guessed — a wrong name yields an empty list indistinguishable from a correct one.
// It returns an empty slice (not an error) when the event has no prior edition.
func (b *AudienceBuilder) ResolvePastEditions(ctx context.Context, eventTerm, locationTerm, currentYear string) ([]string, error) {
	if b.snowflake == nil {
		// Not an error: the caller degrades to a country-only audience and records the gap.
		return nil, nil
	}
	// ResolvePastEventNames REQUIRES a 4-digit year to guarantee "past editions only". When the
	// brief carries none, fall back to the current year rather than passing a blank — a blank is
	// rejected outright, and the fallback keeps the exclusion honest.
	year := strings.TrimSpace(currentYear)
	if !isFourDigitYear(year) {
		year = fmt.Sprintf("%d", time.Now().UTC().Year())
	}
	events, err := b.snowflake.ResolvePastEventNames(ctx, eventTerm, locationTerm, year)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(events))
	for _, e := range events {
		if n := strings.TrimSpace(e.EventName); n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// CreateList creates one DYNAMIC contact list in the project's HubSpot portal.
func (b *AudienceBuilder) CreateList(ctx context.Context, projectID, name string, filter json.RawMessage) (string, error) {
	client, err := b.client(ctx, projectID)
	if err != nil {
		return "", err
	}
	l, cerr := client.CreateList(ctx, name, filter)
	if cerr != nil {
		// Pass the error through unwrapped so an UNCONFIRMED create (a 2xx with no parseable
		// list id) keeps its "verify before retrying" classification instead of being
		// flattened into a generic failure.
		return "", cerr
	}
	if l == nil {
		return "", fmt.Errorf("hubspot: create list %q returned no list", name)
	}
	return l.ListID, nil
}

// client resolves the project's HubSpot connection and builds a client from it, mirroring
// HubSpotDispatcher.Dispatch — the credentials live per project as encrypted connections.
func (b *AudienceBuilder) client(ctx context.Context, projectID string) (*hubspot.Client, error) {
	if strings.TrimSpace(projectID) == "" {
		// Fail loudly: without a project there is no connection to resolve, and silently
		// picking one would build the audience in the wrong portal.
		return nil, fmt.Errorf("audience build: a project id is required to resolve hubspot credentials")
	}
	res, rerr := b.creds.resolve(ctx, projectID, model.ProviderHubSpot)
	if rerr != nil {
		return nil, rerr
	}
	if res.status != model.StatusActive {
		return nil, fmt.Errorf("hubspot connection for project %s is %s, not active", projectID, res.status)
	}
	var creds hubspotCreds
	if uerr := json.Unmarshal(res.plaintext, &creds); uerr != nil {
		return nil, fmt.Errorf("decode hubspot credentials: %w", uerr)
	}
	if strings.TrimSpace(creds.PrivateAppToken) == "" {
		return nil, fmt.Errorf("hubspot credentials are incomplete (need privateAppToken)")
	}
	return hubspot.NewClient(
		hubspot.Credentials{PrivateAppToken: creds.PrivateAppToken},
		hubspot.AccountConfig{PortalID: res.providerConfig["portal_id"]},
		b.opts...,
	), nil
}

// isFourDigitYear mirrors the warehouse client's own guard so the fallback above produces a
// value it will accept.
func isFourDigitYear(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
