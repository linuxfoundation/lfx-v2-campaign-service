// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package accesscheck asks the platform whether a user holds an OpenFGA relation on a project,
// for the few inputs the gateway cannot gate because they live in a request body rather than
// a route.
//
// Two NATS request/reply calls, both owned by other services:
//
//   - lfx.projects-api.slug_to_uid (lfx-v2-project-service HandleProjectSlugToUID): the
//     request body is the slug; a success reply is the project UID as plain text, and a
//     failure reply is a JSON RPCError ({"code":"not_found"|"internal","message":...}).
//   - lfx.access_check.request (lfx-v2-fga-sync accessCheckHandler, docs/fga-sync-contract.md):
//     the request body is one `object#relation@user` line per check; the reply is one
//     `{request}\t{true|false}` line per check, in no guaranteed order. A failure reply is a
//     bare sentence ("failed to check relationship").
//
// Every answer other than an exact, well-formed grant or denial is an ERROR, never a denial
// and never a grant, so the caller can tell "you may not" (403) from "we could not tell" (503).
//
// One gap the contract leaves open: fga-sync (CacheLayer.buildResponseAndWriteBack) answers
// `false` for a check whose OpenFGA BatchCheck item came back with an error, and does not cache
// that answer. An item-level OpenFGA failure is therefore indistinguishable from a denial on the
// wire. Because the failure is not cached while a genuine denial is, a `false` is re-checked
// once: a transient failure usually clears on the fresh request, and a real denial is answered
// again from the cache, cheaply. Only a second `false` is reported as a denial. A failure that
// persists across both requests still yields a denial (403), which fails closed. Closing the gap
// fully needs fga-sync to emit a distinct verdict for an errored item; this package already
// treats any verdict other than "true" or "false" as ErrUnavailable.
package accesscheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

const (
	// SlugToUIDSubject resolves a project slug to its UID (lfx-v2-project-service).
	SlugToUIDSubject = "lfx.projects-api.slug_to_uid"
	// AccessCheckSubject answers batched OpenFGA checks (lfx-v2-fga-sync).
	AccessCheckSubject = "lfx.access_check.request"

	// RelationGlobalMarketingOps is the LF marketing team's relation, written on every project
	// (lfx-v2-helm model.fga). It is the narrower half of campaign_manager, which is
	// `executive_director or global_marketing_ops`.
	RelationGlobalMarketingOps = "global_marketing_ops"

	// requestTimeout bounds EACH of the two requests. fga-sync gives itself 10s
	// (accessCheckHandlerTimeout); a synchronous create should not wait that long, and a slow
	// answer is reported as unavailable, which the caller can retry.
	requestTimeout = 5 * time.Second
)

// ErrUnavailable wraps every failure to obtain a definite answer.
var ErrUnavailable = errors.New("access check unavailable")

// ErrProjectNotFound marks a slug_to_uid reply that DEFINITELY names no project (its
// RPCError code is "not_found"), as opposed to an outage. It is always wrapped together with
// ErrUnavailable, so a caller that only checks ErrUnavailable is unaffected.
var ErrProjectNotFound = errors.New("project not found")

// Requester is the one NATS capability the checker needs. *nats.Conn satisfies it.
type Requester interface {
	RequestWithContext(ctx context.Context, subject string, data []byte) (*nats.Msg, error)
}

// Checker answers relation checks over NATS.
type Checker struct {
	r Requester
}

// New returns a Checker using r.
func New(r Requester) *Checker {
	return &Checker{r: r}
}

// IsGlobalMarketingOps reports whether username holds global_marketing_ops on the project
// whose slug is projectSlug. A false result is a denial answered twice in a row (see the
// package comment for why a first false is re-checked); every error wraps ErrUnavailable.
func (c *Checker) IsGlobalMarketingOps(ctx context.Context, projectSlug, username string) (bool, error) {
	if c == nil || c.r == nil {
		return false, fmt.Errorf("%w: no NATS connection", ErrUnavailable)
	}
	// The username is interpolated into a line-oriented request. A value carrying whitespace,
	// a control character or the token separators could add a second check line or change the
	// subject of this one, so it is refused rather than escaped: no LFID username contains them.
	if !validUsername(username) {
		return false, fmt.Errorf("%w: principal is not a checkable username", ErrUnavailable)
	}
	uid, err := c.projectUID(ctx, projectSlug)
	if err != nil {
		return false, err
	}
	token := "project:" + uid + "#" + RelationGlobalMarketingOps + "@user:" + username
	ok, err := c.check(ctx, token)
	if err != nil || ok {
		return ok, err
	}
	// A false may be an uncached item-level OpenFGA error (see the package comment); ask once
	// more and take the second answer, whatever it is.
	return c.check(ctx, token)
}

// ProjectUID resolves a project slug to its UID through lfx.projects-api.slug_to_uid. Every
// error wraps ErrUnavailable. It satisfies indexer.ProjectUIDResolver (#590).
func (c *Checker) ProjectUID(ctx context.Context, slug string) (string, error) {
	if c == nil || c.r == nil {
		return "", fmt.Errorf("%w: no NATS connection", ErrUnavailable)
	}
	return c.projectUID(ctx, slug)
}

func (c *Checker) check(ctx context.Context, token string) (bool, error) {
	reply, err := c.request(ctx, AccessCheckSubject, token)
	if err != nil {
		return false, err
	}
	return parseCheckReply(reply, token)
}

func (c *Checker) projectUID(ctx context.Context, slug string) (string, error) {
	reply, err := c.request(ctx, SlugToUIDSubject, slug)
	if err != nil {
		return "", err
	}
	// Accept only a UUID. Anything else, including the JSON RPCError a failure replies with
	// and an empty body, is not an answer.
	uid := strings.TrimSpace(reply)
	if _, perr := uuid.Parse(uid); perr != nil || len(uid) != 36 {
		var rpc struct {
			Code string `json:"code"`
		}
		if json.Unmarshal([]byte(uid), &rpc) == nil && rpc.Code == "not_found" {
			return "", fmt.Errorf("%w: %w", ErrUnavailable, ErrProjectNotFound)
		}
		return "", fmt.Errorf("%w: project lookup did not return a UID", ErrUnavailable)
	}
	return uid, nil
}

func (c *Checker) request(ctx context.Context, subject, body string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	msg, err := c.r.RequestWithContext(reqCtx, subject, []byte(body))
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrUnavailable, subject, err)
	}
	if msg == nil {
		return "", fmt.Errorf("%w: %s: empty reply", ErrUnavailable, subject)
	}
	return string(msg.Data), nil
}

// parseCheckReply finds token's own line. Lines are matched on the full token, never by
// position, because fga-sync returns cached results first. A reply with no line for the
// token, or with one whose verdict is neither "true" nor "false", is not an answer.
func parseCheckReply(reply, token string) (bool, error) {
	for _, line := range strings.Split(reply, "\n") {
		req, verdict, ok := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		if !ok || req != token {
			continue
		}
		switch verdict {
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return false, fmt.Errorf("%w: unrecognised access check verdict", ErrUnavailable)
		}
	}
	return false, fmt.Errorf("%w: access check reply did not answer the request", ErrUnavailable)
}

func validUsername(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '#' || r == '@' {
			return false
		}
	}
	return true
}
