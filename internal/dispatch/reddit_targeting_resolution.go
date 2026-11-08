// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Reddit community validation and interest resolution before create (#549, gaps 4 and 8). The
// lookups live in the client (reddit.ResolveCommunities / ResolveInterests); this file adds the
// deployment gate and runs them twice:
//   - in PreflightCreate, so an unknown community or interest is a synchronous 400 before any
//     job exists. A lookup that cannot run there fails open, like the HubSpot pre-check: the
//     orchestrator logs it and the create proceeds to Dispatch;
//   - in Dispatch, before the campaign is created, where they fail CLOSED: an unknown value and
//     a lookup that cannot be read both fail the job with nothing created, so an unverified name
//     or a human label is never sent.

// redditTargetingResolutionEnabled reports whether this deployment opted in. Read per call, like
// redditCustomAudiencesEnabled, so it flips without a rebuild.
func redditTargetingResolutionEnabled() bool {
	return os.Getenv(constants.EnvRedditTargetingResolutionEnabled) == "true"
}

// resolveRedditTargeting resolves cfg's subreddits and interests through client: communities in
// Reddit's spelling, interests as ids. Nothing to resolve returns the lists unchanged. A value
// Reddit does not offer is a domain.PreflightRefusal; a failed lookup is a plain error.
func resolveRedditTargeting(ctx context.Context, client *reddit.Client, cfg redditConfig) (subreddits, interests []string, err error) {
	subreddits, interests = cfg.Subreddits, cfg.Interests
	if entries := reddit.NormalizeSubredditEntries(cfg.Subreddits); len(entries) > 0 {
		if subreddits, err = client.ResolveCommunities(ctx, entries); err != nil {
			return nil, nil, err
		}
	}
	if len(cfg.Interests) > 0 {
		if interests, err = client.ResolveInterests(ctx, cfg.Interests); err != nil {
			return nil, nil, err
		}
	}
	return subreddits, interests, nil
}

// needsRedditTargetingResolution reports whether this create has anything to resolve and the
// deployment opted in.
func needsRedditTargetingResolution(cfg redditConfig) bool {
	return redditTargetingResolutionEnabled() && (len(reddit.NormalizeSubreddits(cfg.Subreddits)) > 0 || len(cfg.Interests) > 0)
}

// upstreamConsultedRefusal marks a config refusal that a platform read produced, so the
// orchestrator records the read as a successful upstream call (it answered) rather than treating
// the refusal as one decided on the config alone.
type upstreamConsultedRefusal struct{ err error }

func (e *upstreamConsultedRefusal) Error() string           { return e.err.Error() }
func (e *upstreamConsultedRefusal) Unwrap() error           { return e.err }
func (e *upstreamConsultedRefusal) UpstreamConsulted() bool { return true }

// preflightRedditTargeting is PreflightCreate's half: a refusal becomes the synchronous 400
// (domain.ErrCreateConfigInvalid, marked as upstream-consulted); any other failure is returned
// as is, which the orchestrator treats as a pre-check that could not be completed (fail open).
func (d *RedditDispatcher) preflightRedditTargeting(ctx context.Context, projectID string, platform model.Provider, cfg redditConfig) error {
	// The list caps are decided on the config alone, before any credential or Reddit call.
	if n := len(reddit.NormalizeSubredditEntries(cfg.Subreddits)); n > reddit.MaxResolvedSubreddits {
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig.subreddits: at most %d distinct communities are accepted, got %d", domain.ErrCreateConfigInvalid, reddit.MaxResolvedSubreddits, n))
	}
	if n := len(cfg.Interests); n > reddit.MaxResolvedInterests {
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig.interests: at most %d entries are accepted, got %d", domain.ErrCreateConfigInvalid, reddit.MaxResolvedInterests, n))
	}
	client, _, err := d.resolveRedditClientWithCreds(ctx, projectID, platform, d.creds.resolve)
	if err != nil {
		return fmt.Errorf("reddit targeting pre-check: %w", err)
	}
	if _, _, err := resolveRedditTargeting(ctx, client, cfg); err != nil {
		var refusal *domain.PreflightRefusal
		if errors.As(err, &refusal) {
			// The client's text reads "invalid redditConfig.<field>: ..."; the 400 is
			// "invalid campaign config: redditConfig.<field>: ...", the shape every other
			// create refusal uses.
			msg := strings.TrimPrefix(refusal.OperatorMessage(), "invalid ")
			return &upstreamConsultedRefusal{err: domain.WrapPreflightRefusal(fmt.Errorf("%w: %s", domain.ErrCreateConfigInvalid, msg))}
		}
		return fmt.Errorf("reddit targeting pre-check: %w", err)
	}
	return nil
}
