// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package assetsign mints and verifies the short-lived, HMAC-signed public URLs at which the
// service serves a stored creative asset to Reddit (LFXV2-2665).
//
// Reddit Ads API v3 takes a promoted-post image only as a public URL it fetches and re-hosts
// at post creation. A signed URL lets a Reddit dispatch use an image stored through
// upload-creative-asset without making that store public: the URL names exactly one asset of
// one brief of one project, is valid for minutes, and is minted only server-side, immediately
// before the Reddit post request that hands it to Reddit — never returned to an API client. The full security model is in
// docs/knowledge/architecture/signed-creative-asset-urls.md.
//
// # Token
//
//	sig = base64url_nopad( HMAC-SHA256( key, canonical(project_id, brief_id, asset_id, exp) ) )
//
// where canonical() is a fixed domain-separation label followed by each id as a 4-byte
// big-endian length plus its bytes, then exp as an 8-byte big-endian unsigned integer.
// Length-prefixing makes the encoding unambiguous — no choice of ids can make two different
// tuples produce the same message, which a delimiter-joined string cannot promise once an id
// may contain the delimiter. The label binds the MAC to this one purpose, so the key cannot be
// coaxed into signing anything else even if it were ever reused.
package assetsign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// MinKeyBytes is the shortest accepted signing key: the HMAC-SHA256 block of security.
	MinKeyBytes = 32
	// DefaultTTL is how long a minted URL is valid when no TTL is configured. Reddit fetches
	// the image during the post-create call it is handed to, so minutes are ample.
	DefaultTTL = 10 * time.Minute
	// MaxTTL is the hard ceiling on a URL's lifetime, both for the configured TTL and for any
	// expiry Verify will accept (plus MaxClockSkew) — a validly signed URL claiming a later
	// expiry is refused, so a leaked key cannot mint long-lived URLs this service would honour.
	MaxTTL = 60 * time.Minute
	// MaxClockSkew is how far AHEAD of the verifying pod's clock the minting pod's clock may
	// be. Verify accepts an expiry up to MaxTTL+MaxClockSkew away, because a URL minted at the
	// MaxTTL ceiling by a pod whose clock leads the serving pod's by even one second would
	// otherwise look "too far in the future" and be refused, failing Reddit's fetch. It widens
	// only the far bound: an expiry already in the past is still refused (a minting pod whose
	// clock LAGS only shortens a URL's life, which MinTTL's margin covers), and it does not let
	// a leaked key mint materially longer-lived URLs. One minute is generous for NTP-synced pods.
	MaxClockSkew = 1 * time.Minute
	// MinTTL is the floor on the configured TTL. The URL is minted immediately before the
	// promoted-post request that hands it to Reddit (reddit.CampaignInput.ImageURLSource), and
	// Reddit fetches it inside that request, so the URL must outlive: one post attempt (bounded
	// by the Reddit client's 30s request timeout), a token refresh that attempt may make first
	// (the cached token is normally still valid, but a 401 earlier in CreateCampaign can
	// invalidate it; the refresh is at most one more request timeout), Reddit's own fetch of
	// the image, and clock skew between the pod that minted it and the pod that serves it.
	// Five minutes covers that with a wide margin; anything shorter risks a created_degraded
	// campaign with no ad because the fetch arrived after expiry.
	MinTTL = 5 * time.Minute

	// RoutePrefix is the path the serving route is mounted under
	// (design/signed_asset.go). Kept here so Mint and the route cannot drift apart silently;
	// TestMintedURLMatchesTheRoute pins them together.
	RoutePrefix = "/_campaigns/creative-assets/"

	// macLabel is the domain-separation prefix of every signed IMAGE message. It is unchanged
	// since LFXV2-2665, so every URL minted before video support still verifies as an image.
	macLabel = "lfx-v2-campaign-service/creative-asset-url/v1"

	// macLabelVideo is the domain-separation prefix of every signed VIDEO message (#549). A
	// distinct label, not a flag inside the message, so an image signature can never verify as
	// a video one or the reverse: the same (project, brief, asset, exp) signs to unrelated MACs
	// under the two labels.
	macLabelVideo = "lfx-v2-campaign-service/creative-asset-url/video/v1"

	// KindParam is the query parameter a VIDEO URL carries (KindParam=KindVideoValue). An
	// image URL carries none, which keeps every pre-#549 URL byte-identical.
	KindParam = "kind"
	// KindVideoValue is KindParam's only value: the route's Enum (design/signed_asset.go).
	KindVideoValue = "video"

	// sigLen is the base64url (no padding) length of a 32-byte MAC.
	sigLen = 43
)

// ProjectIDPattern is what a project id must look like to be put in a signed URL. It is the
// route's own Pattern (design/signed_asset.go), so a URL this package mints always decodes,
// and it admits no character that could traverse or re-root a path.
var ProjectIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// Kind is the slot a signed URL is minted for, and so the only stored type the route will serve
// at it (#549). The zero value is KindImage: the route serves PNG or JPEG and refuses a video.
// KindVideo is minted only for a Reddit VIDEO post's media and serves only video/mp4. The kind is
// bound into the MAC through its label, so a holder of an image URL cannot fetch a video by adding
// kind=video, and a holder of a video URL cannot fetch it as an image by removing it.
type Kind int

const (
	// KindImage serves PNG/JPEG only. Every URL minted before #549 is this kind.
	KindImage Kind = iota
	// KindVideo serves video/mp4 only.
	KindVideo
)

// KindFromParam maps the route's optional kind parameter to a Kind: absent is KindImage,
// KindVideoValue is KindVideo, and anything else is not a kind this package mints (false).
func KindFromParam(p *string) (Kind, bool) {
	if p == nil {
		return KindImage, true
	}
	if *p == KindVideoValue {
		return KindVideo, true
	}
	return KindImage, false
}

func (k Kind) label() (string, bool) {
	switch k {
	case KindImage:
		return macLabel, true
	case KindVideo:
		return macLabelVideo, true
	default:
		return "", false
	}
}

// macEqual is hmac.Equal, held in a variable only so a test can prove the verify path goes
// through the constant-time comparison rather than bytes.Equal or ==.
var macEqual = hmac.Equal

// Signer mints and verifies signed creative-asset URLs. A nil *Signer means the feature is
// OFF: Mint refuses and Verify rejects everything.
type Signer struct {
	key  []byte
	base *url.URL
	ttl  time.Duration
	now  func() time.Time
}

// ErrDisabled is returned by Mint on a nil Signer.
var ErrDisabled = errors.New("signed creative-asset URLs are not enabled")

// New builds a Signer from the raw configuration values.
//
// EVERY non-empty value is validated first, and only then is the feature decided on or off.
// A value that IS set but unusable is an error even when the feature would be off anyway (a
// malformed key with no base URL yet, a 61m TTL with no key): a security control that starts
// silently on a typo only fails later, when the missing half is supplied, which is the worst
// time to learn it. The rules:
//   - the key must be standard base64 decoding to at least MinKeyBytes bytes;
//   - the base URL must be absolute https with a host, and carry no userinfo, path, query or
//     fragment (the route's path is fixed, and anything else would be signed into nothing);
//   - the TTL, when set, must parse as a Go duration within [MinTTL, MaxTTL].
//
// It then returns (nil, nil) — the feature OFF — when the key or the base URL is empty: the
// feature is optional, and a deployment that has not provisioned the secret must still start.
// Error messages never quote the key.
func New(keyB64, baseURL, ttlRaw string) (*Signer, error) {
	keyB64 = strings.TrimSpace(keyB64)
	baseURL = strings.TrimSpace(baseURL)
	var errs []error
	var key []byte
	if keyB64 != "" {
		k, err := base64.StdEncoding.DecodeString(keyB64)
		switch {
		case err != nil:
			errs = append(errs, errors.New("creative-asset URL signing key is not valid standard base64"))
		case len(k) < MinKeyBytes:
			errs = append(errs, fmt.Errorf("creative-asset URL signing key decodes to %d bytes; at least %d are required", len(k), MinKeyBytes))
		default:
			key = k
		}
	}
	var base *url.URL
	if baseURL != "" {
		b, err := parseBaseURL(baseURL)
		if err != nil {
			errs = append(errs, err)
		}
		base = b
	}
	ttl, err := parseTTL(ttlRaw)
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if keyB64 == "" || baseURL == "" {
		return nil, nil
	}
	return &Signer{key: key, base: base, ttl: ttl, now: time.Now}, nil
}

// ValidateIDs reports whether Mint would accept these ids, without minting. The Reddit
// dispatch calls it before any upstream call so that minting LATE — immediately before the
// promoted-post request — cannot fail on an id that was knowable up front.
func ValidateIDs(projectID, briefID, assetID string) error {
	if !ProjectIDPattern.MatchString(projectID) {
		return errors.New("project id cannot be carried in a signed asset URL")
	}
	if _, err := uuid.Parse(briefID); err != nil {
		return errors.New("brief id is not a valid UUID")
	}
	if _, err := uuid.Parse(assetID); err != nil {
		return errors.New("asset id is not a valid UUID")
	}
	return nil
}

func parseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("creative-asset public base URL is not a valid URL")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, errors.New("creative-asset public base URL must use https")
	}
	if u.Hostname() == "" {
		return nil, errors.New("creative-asset public base URL must include a host")
	}
	if u.User != nil {
		return nil, errors.New("creative-asset public base URL must not carry userinfo")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("creative-asset public base URL must be an origin only (no path, query or fragment)")
	}
	return &url.URL{Scheme: "https", Host: u.Host}, nil
}

func parseTTL(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultTTL, nil
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("creative-asset URL TTL %q is not a valid duration", raw)
	}
	if ttl < MinTTL || ttl > MaxTTL {
		return 0, fmt.Errorf("creative-asset URL TTL %s must be between %s and %s", ttl, MinTTL, MaxTTL)
	}
	return ttl, nil
}

// WithClock returns a copy of s that reads time from now. For tests.
func (s *Signer) WithClock(now func() time.Time) *Signer {
	if s == nil {
		return nil
	}
	c := *s
	c.now = now
	return &c
}

// Enabled reports whether s can mint URLs.
func (s *Signer) Enabled() bool { return s != nil }

// TTL is the lifetime of a URL minted now.
func (s *Signer) TTL() time.Duration {
	if s == nil {
		return 0
	}
	return s.ttl
}

// Mint returns the signed URL for one asset and the expiry it carries.
//
// briefID and assetID must be UUIDs; they are rendered in canonical lowercase form so the
// signed bytes and the served path can never disagree on spelling. projectID must match
// ProjectIDPattern. The returned URL is a bearer credential for one image until expiry:
// callers must not log it (log the asset id and expiry instead).
func (s *Signer) Mint(projectID, briefID, assetID string) (string, time.Time, error) {
	return s.MintKind(KindImage, projectID, briefID, assetID)
}

// MintKind is Mint for a given slot kind (#549). KindImage yields exactly Mint's URL; KindVideo
// signs under the video label and adds kind=video, so the route serves the asset only if it is a
// stored video/mp4.
func (s *Signer) MintKind(kind Kind, projectID, briefID, assetID string) (string, time.Time, error) {
	if s == nil {
		return "", time.Time{}, ErrDisabled
	}
	if _, ok := kind.label(); !ok {
		return "", time.Time{}, errors.New("unknown signed asset URL kind")
	}
	if err := ValidateIDs(projectID, briefID, assetID); err != nil {
		return "", time.Time{}, err
	}
	b, a := uuid.MustParse(briefID), uuid.MustParse(assetID)
	exp := s.now().Add(s.ttl).Unix()
	sig := base64.RawURLEncoding.EncodeToString(s.mac(kind, projectID, b.String(), a.String(), exp))
	u := *s.base
	u.Path = RoutePrefix + projectID + "/" + b.String() + "/" + a.String()
	u.RawQuery = "exp=" + strconv.FormatInt(exp, 10)
	if kind == KindVideo {
		u.RawQuery += "&" + KindParam + "=" + KindVideoValue
	}
	u.RawQuery += "&sig=" + sig
	return u.String(), time.Unix(exp, 0), nil
}

// Verify reports whether sig is this server's signature over exactly (projectID, briefID,
// assetID, exp) AND exp is still in the future AND no further away than MaxTTL+MaxClockSkew.
//
// The ids are compared exactly as given, with no normalisation: Mint signs the canonical
// spelling, so any other spelling of the same UUID is a different message and fails. The MAC
// is compared with hmac.Equal (constant time), and it is computed even for an expired or
// over-long expiry so the rejection does not time differently by cause.
//
// It returns a bare bool on purpose: the serving route answers every rejection with the same
// 404, so there is no reason worth carrying out of here.
func (s *Signer) Verify(projectID, briefID, assetID string, exp int64, sig string) bool {
	return s.VerifyKind(KindImage, projectID, briefID, assetID, exp, sig)
}

// VerifyKind is Verify for a given slot kind (#549): the signature must have been minted for
// exactly that kind. An unknown kind verifies nothing.
func (s *Signer) VerifyKind(kind Kind, projectID, briefID, assetID string, exp int64, sig string) bool {
	if s == nil {
		return false
	}
	if _, ok := kind.label(); !ok {
		return false
	}
	if len(sig) != sigLen {
		return false
	}
	got, err := base64.RawURLEncoding.Strict().DecodeString(sig)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	ok := macEqual(s.mac(kind, projectID, briefID, assetID, exp), got)
	now := s.now().Unix()
	fresh := exp > now && exp-now <= int64((MaxTTL+MaxClockSkew)/time.Second)
	return ok && fresh
}

// mac computes the HMAC over the canonical encoding described in the package doc, under the
// kind's label (callers have already refused an unknown kind).
func (s *Signer) mac(kind Kind, projectID, briefID, assetID string, exp int64) []byte {
	label, _ := kind.label()
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(label))
	m.Write([]byte{0})
	for _, f := range []string{projectID, briefID, assetID} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(f))) //nolint:gosec // ids are bounded far below 4 GiB by the route's patterns
		m.Write(n[:])
		m.Write([]byte(f))
	}
	var e [8]byte
	binary.BigEndian.PutUint64(e[:], uint64(exp)) //nolint:gosec // a negative exp encodes distinctly and is refused as expired anyway
	m.Write(e[:])
	return m.Sum(nil)
}
