// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

// metaCreds is the credential shape stored (encrypted) for a Meta connection. Meta
// authenticates with a single long-lived OAuth2 access token.
// metaCreds mirrors MetaAdsCredentials's field name (no json tag) — the persisted
// JSON key is the Go field name (AccessToken), see redditCreds.
type metaCreds struct {
	AccessToken string
}

// metaConfig is the per-platform campaign config the caller passes for Meta in
// CreateCampaigns' Input.Config (delivered here as the Dispatch `config`).
//
// Budget is in whole units of the ad ACCOUNT's currency (NOT USD — the client does
// no FX conversion). CurrencyOffset optionally overrides the account's minor-unit
// scale; when zero the client derives it from the account's ISO currency during its
// preflight.
type metaConfig struct {
	Budget         float64        `json:"budget"`
	LifetimeBudget bool           `json:"lifetimeBudget"`
	StartDate      string         `json:"startDate"` // YYYY-MM-DD
	EndDate        string         `json:"endDate"`   // YYYY-MM-DD
	Objective      string         `json:"objective"` // awareness|traffic|engagement|leads|conversions
	GeoTargets     []string       `json:"geoTargets"`
	Placements     meta.Placement `json:"placements"`
	PixelID        string         `json:"pixelId"`
	// InstagramUserID (IGSID) binds the ad creative to an Instagram account. REQUIRED
	// when any Instagram placement is used (the default placements include Instagram
	// Feed) — without it Meta refuses to publish the ad with "Please add Instagram
	// account". Left empty for Facebook-only campaigns.
	InstagramUserID string `json:"instagramUserId"`
	// DSABeneficiary and DSAPayor are the EU DSA advertiser/payer disclosures. Required
	// for a launch-ready ad set that targets a regulated location; Meta blocks publish
	// ("Please add Advertiser" / "Please add Payer") until both are set.
	DSABeneficiary string           `json:"dsaBeneficiary"`
	DSAPayor       string           `json:"dsaPayor"`
	Variants       []meta.AdVariant `json:"variants"`
	// CurrencyOffset is a FALLBACK minor-unit scale (1 for zero-decimal currencies like
	// JPY, 100 for most), NOT an unconditional override: the client's preflight derives
	// the offset from the account's currency and that is authoritative — a supplied value
	// is used only when the currency can't be resolved, and a value conflicting with a
	// recognized account currency is REJECTED by the client during dispatch. Because
	// CreateCampaigns is asynchronous (a 202 is returned before dispatch runs), that
	// rejection fails the platform job BEFORE any mutating Meta call — it is a pre-create
	// dispatch failure, not a synchronous 4xx on the campaign request. Left 0 → derived.
	CurrencyOffset int64 `json:"currencyOffset"`
}

// creativeAssetReader is the ONE creative-asset operation this dispatcher needs: read a
// stored asset's bytes. It deliberately does not embed domain.CreativeAssetRepository —
// the dispatcher must be able to READ an asset, never to create one. Least privilege,
// exactly as the HubSpot dispatcher's audienceReader narrows the audience repository.
type creativeAssetReader interface {
	GetAsset(ctx context.Context, projectID, briefID, assetID string) (*model.CreativeAsset, error)
	// GetAssetSize prices an asset before it is loaded, so the aggregate reservation can be
	// taken BEFORE the blob is materialised. Reserving afterwards bounds nothing: the memory
	// is already resident by the time the semaphore is consulted.
	GetAssetSize(ctx context.Context, projectID, briefID, assetID string) (int64, error)
}

// MetaDispatcher creates Meta (Facebook/Instagram) campaigns for the orchestrator.
type MetaDispatcher struct {
	creds *credsSource
	// creatives resolves a variant's imageAssetId to image bytes at dispatch. Bound by
	// registerDispatchers on the live and cold-start paths; nil in the direct-construction
	// tests that create no image-by-asset variants, where it is never read
	// (resolveVariantAssets touches it only for a variant that references an asset).
	creatives creativeAssetReader
	// assets bounds the creative-asset bytes CONCURRENT dispatches may hold. Bound by
	// registerDispatchers alongside creatives; nil in direct-construction tests, where it
	// reserves nothing (see AssetReserver). Without it maxVariantAssetBytes caps one
	// dispatch while five run at once — the aggregate this closes.
	assets *AssetReserver
	opts   []meta.Option
}

// NewMetaDispatcher builds the adapter from the connection repo + encryptor. The
// creative-asset read path is bound separately via SetCreativeAssetRepo (see
// BriefService.SetCreativeAssetRepo for the same opt-in shape), so the existing
// direct-construction tests that create no asset-backed variants stay unchanged.
func NewMetaDispatcher(repo connReader, enc domain.Encryptor, opts ...meta.Option) *MetaDispatcher {
	return &MetaDispatcher{creds: newCredsSource(repo, enc), opts: opts}
}

// SetCreativeAssetRepo binds the creative-asset read path so image-referencing variants
// resolve to bytes at dispatch. registerDispatchers calls it once at construction, before
// the dispatcher is shared with the orchestrator, so no lock guards it (unlike
// BriefService, which late-binds across a cold start). A nil argument is ignored —
// mirroring BriefService.SetCreativeAssetRepo — leaving the dispatcher image-less rather
// than storing a typed-nil that would panic when a variant referenced an asset.
func (d *MetaDispatcher) SetCreativeAssetRepo(r creativeAssetReader) {
	if r == nil {
		return
	}
	d.creatives = r
}

// SetAssetReserver late-binds the PROCESS-WIDE creative-asset memory budget, mirroring
// SetCreativeAssetRepo's opt-in shape. A nil reserver reserves nothing, so the
// direct-construction tests are unaffected.
func (d *MetaDispatcher) SetAssetReserver(a *AssetReserver) {
	if a == nil {
		return
	}
	d.assets = a
}

// AssetReserverIsSet reports whether the aggregate asset budget was bound.
//
// Exported for exactly the reason CreativeAssetRepoIsSet is: the container's wiring tests
// construct dispatchers with all-nil arguments, so deleting registerDispatchers' bind
// COMPILES and leaves the suite green while production loses the aggregate bound. Only an
// assertion on the bound state itself holds the wiring.
func (d *MetaDispatcher) AssetReserverIsSet() bool {
	return d.assets != nil
}

// CreativeAssetRepoIsSet reports whether the creative-asset read path was bound.
// Exported ONLY so the container's wiring tests can assert the binding directly, for
// exactly the reason BriefService.CreativeAssetRepoIsSet is (see
// internal/service/creative_asset.go): the container tests construct dispatchers with
// all-nil arguments, so deleting registerDispatchers' SetCreativeAssetRepo call
// COMPILES and leaves the whole suite green — while every asset-backed production
// dispatch fails with "the creative-asset store is not configured", i.e. no ad at all
// for a brief whose creative was uploaded successfully.
//
// An error-based assertion cannot close this gap: a dispatcher that was never bound
// and one bound to a repo that cannot find the asset both fail the variant, and the
// no-database container legitimately produces the unbound state. Only asking the
// dispatcher directly distinguishes wired from unwired.
func (d *MetaDispatcher) CreativeAssetRepoIsSet() bool { return d.creatives != nil }

// maxVariantAssetBytes bounds the total DISTINCT creative-asset bytes one dispatch may
// hold in memory at once.
//
// It is aligned with the caps PR #170 already established rather than inventing a third
// scheme. Those are all derived from ONE number — the 30 MiB per-asset ceiling enforced on
// the upload (internal/service's maxCreativeStoredBytes, itself set at Meta's documented
// single-image maximum; the design's MaxLength(41943040) is that same ceiling expressed in
// base64 CHARACTERS, which is the unit the wire schema measures): constants.MaxRequestBodyBytes
// is 42 MiB (one 30 MiB image base64-expanded by 4/3, plus envelope) and the decode budget is
// 80 MiB. This is the same ceiling applied to the one code path that holds SEVERAL assets
// simultaneously.
//
// 240 MiB is EIGHT maximum-size (30 MiB) assets. Justified against a legitimate campaign:
// real Meta creatives are nothing like 30 MiB — Meta's own recommended feed image is
// 1936x1936, which is a few hundred KiB as PNG or JPEG, so 240 MiB is several hundred
// realistic creatives. A/B tests in this service run a handful of variants, not hundreds,
// and a campaign that genuinely needs more distinct artwork than this is better split than
// dispatched as one job. The bound therefore refuses only configs that are already
// pathological, while capping what a single dispatch can allocate at a fixed, modest
// multiple of the largest thing the upload contract admits.
//
// Deduplication (below) is what makes the bound meaningful: without it the SAME asset id
// repeated N times would charge N times over, so the cheapest attack would exhaust the
// budget using one stored image.
//
// The bound is on bytes RETAINED, and the check runs after the tripping asset has been read,
// so the true peak is this value plus one maximum-size asset — 270 MiB, not 240. See the
// check in resolveVariantAssets for why that overshoot is accepted rather than closed.
//
// IT BOUNDS ONE DISPATCH ONLY. The aggregate across concurrent dispatches is
// MaxConcurrentVariantAssetBytes below; this constant said nothing about how many dispatches
// run at once, and five of them did.
const maxVariantAssetBytes int64 = 240 << 20 // 240 MiB = 8 maximum-size (30 MiB) assets

// MaxConcurrentVariantAssetBytes is the total creative-asset memory ALL concurrent dispatches
// may hold at once, the aggregate companion to maxVariantAssetBytes.
//
// THE DEFECT IT CLOSES. maxParallelDispatch (internal/service/orchestrator.go) is 5, and that
// semaphore is process-wide across all jobs with NO per-provider partition, so every slot can be
// a Meta dispatch. Five at the per-dispatch peak is 5 x 270 MiB = 1.32 GiB against a 512 MiB
// pod — 2.6x. It does not take the worst case: TWO asset-heavy dispatches already exceed the
// pod before multipart copies and ordinary process memory.
//
// WHY IT EQUALS maxVariantAssetBytes RATHER THAN A SMALLER NUMBER THAT MAKES 5 FIT. Sizing this
// to satisfy the five-way arithmetic would put it below the per-dispatch cap, and then a single
// config carrying eight maximum-size assets — legal today, and accepted by the per-dispatch
// ceiling — could never acquire and would be refused. A bound that meets its arithmetic by
// rejecting work the contract accepts is not a fix; it is the same error shape as pricing a
// permit so cheaply that nothing legal fits, which this service has already made once.
//
// Equal to the per-dispatch cap is the SMALLEST value that refuses no legal config: exactly one
// maximum-size dispatch fits, and every smaller one shares the remainder — the same "priced for
// the worst legal input, shared by everything smaller" shape as the upload and decode budgets.
// What it removes is the MULTIPLIER, which is where the 2.6x came from:
//
//	before: 5 x (240 MiB + 30 MiB materialised) = 1.32 GiB = 2.6x the pod
//	after:      240 MiB + 30 MiB materialised   =  270 MiB = 53% of the pod
//
// It is NOT derived from PodMemoryLimitBytes the way the upload and decode budgets are, and that
// is deliberate: those bound HTTP-request memory and are sized as fractions of the pod, while
// this one is pinned to the per-dispatch contract because the binding constraint is "one legal
// dispatch must always fit", not a share of the pod. Lowering maxVariantAssetBytes lowers this
// in step, which is the intended coupling.
const MaxConcurrentVariantAssetBytes int64 = maxVariantAssetBytes

// VariantAssetReserveWait bounds how long a dispatch waits for asset budget before it is
// refused.
//
// Longer than the HTTP-side admission waits (250ms) because the trade is different: a dispatch is
// a background job with no client on a socket, so a short wait buys nothing and a queued dispatch
// is cheaper than a failed one. It is bounded rather than open-ended so a dispatch cannot sit
// behind another's assets indefinitely — without it, providerCallTimeout would be the only thing
// ending the wait, and it would spend that whole budget queueing instead of dispatching.
const VariantAssetReserveWait = 30 * time.Second

// maxAssetIDInError bounds how much of a REJECTED imageAssetId is quoted back. A valid
// id is a 36-character UUID, so this is generous for anything legitimate while keeping a
// malformed value short enough to stay readable. Deliberately well under
// errSummaryMaxRunes (200) so this value cannot by itself consume an error summary and
// push the surrounding wording — which says WHICH variant and WHAT is wrong — out of the
// truncation window.
const maxAssetIDInError = 64

// safeAssetIDForError renders a rejected imageAssetId for an error message that reaches
// operator-facing output and a structured error log.
//
// imageAssetId is opaque CALLER JSON with no length or charset bound anywhere on its path
// (design/brief.go sets none, and the config is decoded straight into metaConfig), so the
// raw value is attacker-controlled in both size and content. It reaches
// slog.ErrorContext's "error" attribute via notCreated → the orchestrator's default
// pre-create arm, so quoting it unchanged would let a caller write arbitrary text — and
// arbitrarily MUCH of it — into the orchestrator's structured error log.
//
// Two independent problems, so two independent controls:
//   - UNBOUNDED LENGTH → truncated to maxAssetIDInError runes (runes, not bytes, so a
//     multi-byte value is never split mid-character into invalid UTF-8).
//   - LOG INJECTION → every non-graphic rune is replaced. Newlines and carriage returns
//     are the ones that matter: a log line the caller can break is a log line the caller
//     can forge a second, fake entry inside.
//
// It is NOT a redactor and makes no claim to be one — the same distinction
// safeErrSummary carries. It bounds and neutralises; it does not decide that the content
// was secret. Nothing secret is expected here (this is a caller-supplied reference, not a
// credential), so bounding the blast radius is the appropriate control.
//
// The value is wrapped in explicit markers rather than %q. %q would escape the control
// characters, but only AFTER the unbounded value had already been accepted, and it leaves
// the reader unable to tell a truncated value from a complete one.
func safeAssetIDForError(id string) string {
	var b strings.Builder
	n := 0
	truncated := false
	for _, r := range id {
		if n == maxAssetIDInError {
			truncated = true
			break
		}
		if unicode.IsGraphic(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(unicode.ReplacementChar)
		}
		n++
	}
	out := "<" + b.String() + ">"
	if truncated {
		out = "<" + b.String() + "… (truncated)>"
	}
	return out
}

// resolveVariantAssets loads each variant's referenced image (imageAssetId) into the bytes
// the Meta client uploads, returning a COPY so the caller's cfg.Variants — reused by
// campaignFromMeta for the degraded-count check and the config snapshot — is not mutated.
// The copy is for CALLER ISOLATION: those later readers must see the config the caller
// sent, not one this function rewrote. It is not what keeps bytes out of the persisted
// snapshot — meta.AdVariant.ImageBytes is tagged `json:"-"` (internal/platform/meta/
// client.go:2469), so resolved bytes never marshal into config_snapshot regardless.
// A variant with no imageAssetId passes through unchanged (a link-only or by-URL creative).
//
// Every failure here is a caller/wiring error that MUST fail the dispatch BEFORE any
// upstream create — the sole call site wraps the returned error in notCreated so the
// (brief, platform) claim is RELEASED rather than stranded, and the resolution runs before
// meta.NewClient, so no Meta call is made. It is NOT a pre-credential boundary: Dispatch
// calls resolveMetaCredentials as its first statement, which loads, decrypts and decodes
// the stored token before this ever runs. The guarantee is pre-spend, not
// credential-avoidance:
//   - a malformed imageAssetId cannot reference a real asset, so it is rejected up front
//     rather than handed to the UUID primary-key lookup (which would raise an opaque
//     driver error);
//   - a nil repo with an image-referencing variant is a wiring defect (registerDispatchers
//     always binds it) — surfaced as a clear error, not a nil-panic;
//   - an asset absent for THIS brief (missing, or another brief's/project's) is ErrNotFound
//     from GetAsset's scoped lookup, reported as a bad reference.
//
// A bad asset must NOT fall through to a link-only ad: the caller asked for an image, and
// silently creating an imageless ad would spend budget on a creative nobody approved.
// It returns a RELEASE func, always non-nil, so `defer release()` at the call site is safe on
// every path.
//
// ON SUCCESS the reservation is NOT released here and the returned func owns it: the resolved
// bytes are handed back in the variant slice and the Meta client POSTs them to /adimages later
// in the same dispatch, so releasing at this function's return would free budget that is still
// occupied.
//
// ON FAILURE — any error, and any panic — this function releases everything it reserved before
// returning, and the returned func is a no-op. The caller therefore cannot leak by forgetting,
// and a caller that defers unconditionally cannot double-release either (releaseAll is
// idempotent). That asymmetry is deliberate: it used to be the caller's job on both paths, and
// the result was a leak on every error arm.
func (d *MetaDispatcher) resolveVariantAssets(ctx context.Context, brief *model.CampaignBrief, variants []meta.AdVariant) ([]meta.AdVariant, func(), error) {
	out := make([]meta.AdVariant, len(variants))
	copy(out, variants)
	// Resolved assets are CACHED BY ID for the duration of this call, which is what makes
	// the memory bound hold. Nothing caps how many variants a config may carry, and each
	// asset may be 30 MiB (design/brief.go's MaxLength on the upload), so the earlier
	// version's one-read-and-one-30-MiB-buffer PER VARIANT was unbounded in both DB reads
	// and allocation — and the cheapest way to trigger it was the SAME asset id repeated,
	// which needs no extra stored data at all. De-duplication makes repetition free: N
	// variants naming one asset now cost one read and one buffer, and the ImageBytes slices
	// alias that single buffer (they are only ever read, never mutated — the meta client
	// copies them onto the wire).
	//
	// Cost is then bounded by the number of DISTINCT assets, so an aggregate ceiling is
	// still needed for a config naming many different ones. It is deliberately derived from
	// the caps PR #170 already set rather than being a third scheme: that PR bounds ONE
	// request at constants.MaxRequestBodyBytes (42 MiB) and one decode at 80 MiB, both
	// sized from the same 30 MiB per-asset ceiling. maxVariantAssetBytes applies the same
	// idea to the one place that holds SEVERAL assets at once.
	byID := make(map[string][]byte, len(out))
	mimeByID := make(map[string]string, len(out))
	var totalBytes int64

	// Per-asset reservations, released together. Collected rather than folded into one weight
	// because each is acquired separately (before its own load), and a weighted semaphore must
	// be released with exactly the weights it was acquired with — releasing a different total
	// silently corrupts its accounting rather than failing.
	//
	// THE RELEASE IS STRUCTURAL, not a thing each return statement must remember.
	//
	// The previous shape returned releaseAll from every arm and relied on the author of each
	// return — and on the CALLER's defer — to hand it back. That leaked twice over: two arms
	// returned a no-op instead, and the caller's `defer releaseAssets()` sat BELOW its error
	// check, so it never ran on failure at all. Both are the same bug, which is that a
	// correctness property was spread across every exit rather than expressed once.
	//
	// The defer below owns it instead. On ANY error return — including ones added later, and
	// including a panic — it unwinds every reservation taken so far. On success it does nothing
	// and the caller receives the releaser, because the resolved bytes outlive this call: they
	// go back in the variant slice and the Meta client POSTs them to /adimages later in the same
	// dispatch. That is the one case the caller must still own, and it is now the ONLY one.
	var releases []func()
	releaseAll := func() {
		for _, rel := range releases {
			rel()
		}
		releases = nil
	}
	//
	// The flag asks "did this function hand the reservation to the caller", NOT "did it fail",
	// and the difference is the panic path. An `if err != nil` defer (which needs a named error
	// return) skips the release on a PANIC, because err is nil there too — and a panic is the
	// one exit a defer exists to cover in the first place. A flag set immediately before the
	// single successful return covers every other exit as a side effect: any return that is not
	// that one, existing or added later, unwinds.
	succeeded := false
	defer func() {
		if !succeeded {
			releaseAll()
		}
	}()
	for i := range out {
		assetID := strings.TrimSpace(out[i].ImageAssetID)
		if assetID == "" {
			continue
		}
		parsed, perr := uuid.Parse(assetID)
		if perr != nil {
			return nil, func() {}, fmt.Errorf("meta variant %d references creative asset %s, which is not a valid asset id", i+1, safeAssetIDForError(assetID))
		}
		// CANONICALIZE before the id is used as a cache key or a lookup value.
		//
		// uuid.Parse accepts four spellings of the SAME uuid — canonical, braced
		// ({...}), URN (urn:uuid:...) and unhyphenated — so the caller's raw spelling is
		// not a stable identity. Keying the cache on it would let a config reference one
		// asset through several valid aliases and defeat the dedupe entirely: each alias
		// would miss the map, read the row again, retain another buffer, and be charged
		// against the aggregate budget again. That is the exact unbounded case the dedupe
		// exists to prevent, reachable with no extra stored data — and it would eventually
		// refuse a legitimate config with a false "distinct creative assets" rejection
		// naming assets that are not distinct.
		//
		// The canonical form is also what the lookup should carry: the stored primary key
		// is a uuid column, so a braced or URN spelling is this service's spelling
		// problem, not a different asset.
		assetID = parsed.String()
		if b, seen := byID[assetID]; seen {
			// Already resolved for an earlier variant: no second read, no second buffer,
			// and no second charge against the aggregate budget.
			out[i].ImageBytes = b
			out[i].ImageMIME = mimeByID[assetID]
			continue
		}
		if d.creatives == nil {
			return nil, func() {}, fmt.Errorf("meta variant %d references creative asset %s but the creative-asset store is not configured", i+1, assetID)
		}
		// PRICE THE ASSET BEFORE LOADING IT.
		//
		// This read costs one BIGINT and no BYTEA. It exists because the aggregate reservation
		// has to be taken BEFORE the blob is resident — charging afterwards bounds nothing, since
		// every concurrent dispatch would already be holding its full allowance by the time it
		// blocked on the semaphore. That was a real defect in the first version of this bound.
		size, serr := d.creatives.GetAssetSize(ctx, brief.ProjectID, brief.ID, assetID)
		if serr != nil {
			if errors.Is(serr, domain.ErrNotFound) {
				return nil, func() {}, fmt.Errorf("meta variant %d references creative asset %s, which does not exist for this brief", i+1, assetID)
			}
			return nil, func() {}, fmt.Errorf("meta variant %d: size creative asset %s: %w", i+1, assetID, serr)
		}

		// The PER-DISPATCH ceiling, now also applied before the read rather than after it. The
		// asset that trips the ceiling is no longer materialised first, so the old
		// "peak is the cap plus one asset" overshoot is gone.
		totalBytes += size
		if totalBytes > maxVariantAssetBytes {
			return nil, func() {}, fmt.Errorf("meta variants reference more than %d bytes of distinct creative assets (%d bytes at variant %d); split the campaign or reuse fewer images",
				maxVariantAssetBytes, totalBytes, i+1)
		}

		// AGGREGATE reservation for THIS asset, taken before it is loaded. Charged incrementally
		// rather than once at the end for exactly the ordering reason above.
		relOne, ok := d.assets.reserve(ctx, size)
		if !ok {
			return nil, func() {}, fmt.Errorf("meta variants reference %d bytes of creative assets, which exceeds the memory concurrently available for dispatch; retry when other dispatches finish", totalBytes)
		}
		releases = append(releases, relOne)

		asset, gerr := d.creatives.GetAsset(ctx, brief.ProjectID, brief.ID, assetID)
		if gerr != nil {
			if errors.Is(gerr, domain.ErrNotFound) {
				return nil, func() {}, fmt.Errorf("meta variant %d references creative asset %s, which does not exist for this brief", i+1, assetID)
			}
			return nil, func() {}, fmt.Errorf("meta variant %d: load creative asset %s: %w", i+1, assetID, gerr)
		}
		// The reservation was priced from byte_size; the CHECK on that column
		// (migration 000029) ties it to octet_length(bytes), so the two agree by construction.
		// A mismatch would mean the row violates its own constraint, which is a data defect
		// rather than something to silently re-charge for.
		_ = size
		// A resolved asset that carries no bytes cannot become a creative. Refuse rather
		// than proceed: leaving ImageBytes empty here would build a LINK-ONLY ad for a
		// variant that asked for an image — a silent downgrade that spends money.
		if len(asset.Bytes) == 0 {
			return nil, func() {}, fmt.Errorf("meta variant %d references creative asset %s, which has no stored image bytes", i+1, assetID)
		}
		byID[assetID] = asset.Bytes
		mimeByID[assetID] = asset.MimeType
		out[i].ImageBytes = asset.Bytes
		out[i].ImageMIME = asset.MimeType
	}
	// Every asset was reserved BEFORE it was loaded, so by here the aggregate budget already
	// accounts for exactly what this dispatch holds. releaseAll returns all of it at once.
	//
	// A config with no asset-backed variants took no reservation at all, so releaseAll is a
	// no-op over an empty slice — such a dispatch never queues behind one that holds assets.
	// Hand the reservation to the caller: the resolved bytes outlive this call, so the deferred
	// unwind above must NOT fire.
	succeeded = true
	return out, releaseAll, nil
}

// resolveMetaCredentials fetches the project's Meta connection and validates it is usable
// for ANY Meta operation — active status, decodable credentials, non-empty access token —
// tagging each defect with domain.ErrConnectionNotUsable plus a reason sentinel. The pattern —
// named returns, defer systemScoped, a reason sentinel under ErrConnectionNotUsable — ORIGINATES
// in Google Ads' validateGoogleAdsCredentials; resolveRedditClient adopted it from there and
// says so, and is the nearest sibling to read alongside this one. Cite the origin rather than
// the sibling: which adapter you copy from is a detail, which one DEFINES the shape is not.
// It deliberately does NOT check account_id: only Dispatch needs it (a
// campaign create builds Graph paths as /{accountID}/campaigns etc. — see
// internal/platform/meta/client.go's AccountID checks ahead of CreateCampaign). ToggleStatus
// and ReadMetrics target an existing campaign by id (POST /{campaignID}, GET
// /{campaignID}/insights) and never read AccountConfig.AccountID at all, so requiring one
// there would refuse a perfectly servable pause/metrics-read on a connection whose account
// selection was later cleared via PUT.
//
// resolveCreds selects the credential entry point: d.creds.resolve for creation and discovery
// (both governed by the forced-system flag) and d.creds.existingResolver(...) for an operation
// on an already-created campaign, which resolves the account that campaign was CREATED under
// — the project's, or the LF system one when the flag governed its creation (see
// resolveExisting). Passed in rather than inferred, because only the caller knows whether it
// holds a campaign, and only it can read that campaign's recorded creation account.
func (d *MetaDispatcher) resolveMetaCredentials(ctx context.Context, projectID string, platform model.Provider, resolveCreds credsResolver) (res *resolved, creds metaCreds, err error) {
	res, err = resolveCreds(ctx, projectID, platform)
	if err != nil {
		return nil, metaCreds{}, err
	}
	// The defer closes over `conn`, NOT the named return `res`. Every not-usable return
	// below sets res to nil before the defer runs, and systemScoped is a no-op on a nil
	// receiver — so reading the named return here would silently drop the system-row
	// attribution from exactly the errors that need it, on every caller: dispatch, toggle,
	// metrics and discovery alike. Failing open like that is the whole defect systemScoped
	// exists to prevent, and it leaves no trace, because the error is still correct in
	// every other respect. Bind the resolved connection once and read that.
	conn := res
	defer func() { err = conn.systemScoped(err) }()
	if res.status != model.StatusActive {
		return nil, metaCreds{}, fmt.Errorf("%w: %w: meta connection for project %s is %s, not active",
			domain.ErrConnectionNotUsable, domain.ErrConnectionInactive, projectID, res.status)
	}
	if uerr := json.Unmarshal(res.plaintext, &creds); uerr != nil {
		// The cause is DROPPED, not wrapped. It is the only value here derived from the
		// DECRYPTED credential blob, and encoding/json quotes its input in
		// *json.SyntaxError / *json.UnmarshalTypeError. Today's stdlib happens not to quote
		// the offending bytes for a struct of string fields — it reports "invalid character
		// 'T' after object key:value pair", not the input — but that is a behaviour, not a
		// documented guarantee, and it does not hold for every field type: a number decoded
		// into a numeric field appears in the message verbatim. Dropping the cause removes
		// the whole class rather than resting on a property of the stdlib nobody here
		// controls, and it costs nothing, because the sentinel already names the only thing
		// an operator can act on: this connection's stored credential has to be re-entered.
		// The project id below is not plaintext-derived and stays. Matches
		// resolveRedditClient's decode error. This reaches the discovery handler too, which
		// logs it and describes the not-usable arm to the caller.
		return nil, metaCreds{}, fmt.Errorf("%w: %w: meta credentials for project %s are not valid JSON",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsUndecodable, projectID)
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil, metaCreds{}, fmt.Errorf("%w: %w: meta credentials are incomplete (need accessToken)",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete)
	}
	return res, creds, nil
}

// requireMetaAccountID returns res.accountID trimmed, or — when it is empty — an error
// naming the missing choice: domain.ErrAccountNotSelected alongside
// domain.ErrConnectionNotUsable, which unusableConnectionReason reports as
// "account_not_selected". That pair is a CLASSIFICATION, not a status code, and the reason
// token reaches an operator through the LOG — not the job result. Be precise about this,
// because the natural assumption is wrong: the only caller is Dispatch, which is queued work,
// and dispatchPlatform collapses EVERY dispatcher error into the same
// "platform campaign creation failed" job result (internal/service/orchestrator.go). Nothing
// returned here reaches the caller as text. Google Ads' create path is in exactly the same
// position and says so at validateGoogleAdsConnection's call site; classification there buys
// log hygiene and claim semantics, and it buys the same here.
//
// The same sentinels DO drive a synchronous 409 (internal/service/brief.go) — which is why
// they are used rather than a bespoke error — but only from the status toggle and metrics
// read, and only for providers whose toggle/metrics need an account id. Meta's do not (they
// target the campaign node by id), so for Meta this sentinel has no synchronous call site at
// all today. Its value is that the fixed-vocabulary reason token identifies the missing
// choice in the dispatch-failure log line instead of leaving an unclassified error there.
//
// An account-id-less connection (the credentials-only bootstrap state — see
// MetaAdsConnectionConfig in design/connection.go) can create no campaign, and this refuses
// it HERE, before an empty AccountConfig.AccountID can reach the Meta client and fail
// opaquely with a malformed "//campaigns" request instead of a reason naming the fix.
// ToggleStatus and ReadMetrics do not call this — see resolveMetaCredentials for why.
func requireMetaAccountID(res *resolved, projectID string) (string, error) {
	accountID := strings.TrimSpace(res.accountID)
	if accountID == "" {
		return "", res.systemScoped(fmt.Errorf("%w: %w: meta connection for project %s has no account id selected",
			domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected, projectID))
	}
	return accountID, nil
}

// Dispatch implements service.PlatformDispatcher for Meta.
func (d *MetaDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (camp *model.Campaign, err error) {
	// d.creds.resolve, not an existingResolver: Dispatch CREATES, so it is governed by the
	// forced-system flag rather than by an account a campaign was previously created under.
	res, creds, err := d.resolveMetaCredentials(ctx, brief.ProjectID, platform, d.creds.resolve)
	if err != nil {
		return nil, notCreated(err)
	}
	// Record WHICH ACCOUNT served this campaign on every exit that returns a row —
	// including the UNCONFIRMED/degraded paths that return a campaign alongside an error.
	// See stampProvenance for why this is a defer on the named return, not a per-return call.
	defer func() { res.stampProvenance(camp) }()
	accountID, err := requireMetaAccountID(res, brief.ProjectID)
	if err != nil {
		return nil, notCreated(err)
	}
	pageID := strings.TrimSpace(res.providerConfig["page_id"])
	if pageID == "" {
		// page_id is Required at connection creation (design/connection.go), so this is
		// unreachable through normal API validation; it only fires if a row somehow
		// stored an empty value. CreateCampaigns already returned 202 by the time Dispatch
		// runs, so this can't surface as a synchronous 4xx — notCreated marks it
		// NoUpstreamCreate so the orchestrator releases the pending claim instead of
		// retaining it for a create that may have partially landed, and the sentinel/reason
		// chain (ErrConnectionNotUsable/ErrProviderConfigInvalid) is what a human reads back
		// from the async job's failure log, same as every other stored-state defect here.
		return nil, notCreated(res.systemScoped(fmt.Errorf("%w: %w: meta connection for project %s is missing page id",
			domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid, brief.ProjectID)))
	}

	var cfg metaConfig
	if err := unmarshalPlatformConfig(config, "metaConfig", &cfg); err != nil {
		return nil, notCreated(err)
	}
	bf, err := decodeBriefFields(brief)
	if err != nil {
		return nil, notCreated(err)
	}

	account := meta.AccountConfig{
		AccountID:      accountID,
		PageID:         pageID,
		Label:          res.label,
		CurrencyOffset: cfg.CurrencyOffset,
	}
	// hsToken is a documented TOP-LEVEL config envelope field (docs/api-catalog.md —
	// sibling to metaConfig, NOT nested in it), read via the shared envelope helper. A
	// request-supplied token takes precedence over the brief blobs; without this a
	// documented config.hsToken is silently ignored and the client falls back to the
	// event slug for utm_campaign, losing the HubSpot attribution.
	hsToken, err := envelopeHSToken(config)
	if err != nil {
		return nil, notCreated(err) // a wrong-typed hsToken is a caller error (pre-create)
	}
	if hsToken == "" {
		hsToken = bf.HSToken
	}

	// Resolve every variant's referenced image to BYTES before the client exists. Any
	// bad reference (malformed id, unknown/foreign asset, an asset with no bytes) fails
	// the dispatch HERE — notCreated marks it NoUpstreamCreate so the orchestrator
	// RELEASES the pending (brief, platform) claim instead of stranding it, and because
	// this runs before meta.NewClient below, no Meta call is made. The credential is
	// already resolved and decrypted by this point (resolveMetaCredentials runs first),
	// so this is a pre-spend boundary, not a pre-credential one.
	// A variant that carries an image URL instead resolves to nothing here and is
	// attached by the client as link_data.picture; the two are mutually exclusive per
	// variant and the client refuses a variant supplying both.
	// The release is deferred to the END OF THE DISPATCH, not to the resolve, because the
	// resolved bytes live in `variants` and the Meta client POSTs them to /adimages further
	// down this same function. Releasing earlier would hand the budget back while the memory
	// it accounts for is still resident, which is exactly the bound this reservation exists to
	// provide.
	//
	// THE DEFER GOES ABOVE THE ERROR CHECK, and that ordering is the fix for a real leak: it
	// used to sit below, so a failed resolve returned without ever running it. resolveVariantAssets
	// now unwinds its own reservations on error and hands back a no-op, so this defer is
	// harmless on the error path — but it is placed here so the two cannot disagree again.
	// releaseAssets is never nil, on any path.
	variants, releaseAssets, verr := d.resolveVariantAssets(ctx, brief, cfg.Variants)
	defer releaseAssets()
	if verr != nil {
		return nil, notCreated(verr)
	}

	in := meta.CampaignInput{
		EventName: bf.EventName,
		EventSlug: brief.EventSlug,
		// Project stamped from the authenticated scope, not caller JSON (api-catalog).
		Project:         brief.ProjectID,
		RegistrationURL: bf.RegistrationURL,
		HSToken:         hsToken,
		Objective:       cfg.Objective,
		GeoTargets:      cfg.GeoTargets,
		Budget:          cfg.Budget,
		LifetimeBudget:  cfg.LifetimeBudget,
		StartDate:       cfg.StartDate,
		EndDate:         cfg.EndDate,
		Placements:      cfg.Placements,
		PixelID:         cfg.PixelID,
		InstagramUserID: cfg.InstagramUserID,
		DSABeneficiary:  cfg.DSABeneficiary,
		DSAPayor:        cfg.DSAPayor,
		// The RESOLVED variants (image bytes filled in), not cfg.Variants — cfg.Variants
		// stays pristine for campaignFromMeta's snapshot and degraded-count check.
		Variants: variants,
	}

	client := meta.NewClient(meta.Credentials{AccessToken: creds.AccessToken}, account, d.opts...)

	// Release the claim ONLY when result==nil. An ambiguous create (or a post-campaign
	// failure) returns a non-nil partial whose CampaignID may be empty but still means
	// "may exist" — gating on an empty CampaignID would wrongly release the claim.
	result, cerr := client.CreateCampaign(ctx, in)
	if cerr != nil {
		if result == nil {
			return nil, notCreated(fmt.Errorf("meta campaign creation failed before any upstream create: %w", cerr))
		}
		return campaignFromMeta(ctx, result, cfg), fmt.Errorf("meta campaign creation UNCONFIRMED: %w", cerr)
	}
	// Meta creates one ad per requested variant but treats per-variant ad failures as
	// NON-fatal (the client records them in Steps and continues), so a nil error can
	// still come back with AdCount < the number of variants requested — a DEGRADED
	// success. We do NOT return an error: the campaign IS created, so failing the job
	// would mislead and be unrecoverable by retry (idempotency short-circuits a
	// re-dispatch, never re-running the ad steps). Instead the shortfall is made VISIBLE
	// as a distinct created_degraded status (per-variant failures are in Result.Steps)
	// for a human/monitor to reconcile. Mirrors the reddit/twitter partial-ad handling.
	// All requested variants are valid here (the client fails fast on a malformed
	// variant), so len(cfg.Variants) is the requested count.
	camp = campaignFromMeta(ctx, result, cfg)
	if result.AdCount < len(cfg.Variants) {
		camp.Status = campaignStatusCreatedDegraded
	}
	return camp, nil
}

// ToggleStatus pauses or resumes an existing Meta campaign on the platform. It resolves the
// connection (an inactive/undecryptable/incomplete connection is a clean 409, not a 503 —
// see resolveMetaCredentials), builds the client, and CASCADES the status to the campaign,
// its ad set, and every ad — Meta's create PAUSES all three, so toggling only the campaign
// to ACTIVE would not serve. campaign is the persisted row; the ad set id is read from its
// CampaignResult, and the ads are enumerated live via GET /{adSetID}/ads rather than from
// CampaignResult.Ads (which LFXV2-3295 does persist) — see UpdateCampaignAndChildrenStatus
// for why discovery is the deliberate choice. status is model.CampaignRunActive or
// model.CampaignRunPaused. Returns nil only when the platform confirms; an UNCONFIRMED
// outcome (including a partial cascade) is wrapped so the caller reports "verify before
// retry" (via the Unconfirmed() behavioral interface).
func (d *MetaDispatcher) ToggleStatus(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, status string) error {
	metaStatus, err := metaRunStatus(status)
	if err != nil {
		return err
	}
	res, creds, err := d.resolveMetaCredentials(ctx, projectID, platform, d.creds.existingResolver(metaCreationAccountID(campaign)))
	if err != nil {
		return err
	}
	// A status update targets the campaign node by id (POST /{campaignID}); it needs
	// neither page id nor account id (unlike Dispatch — see resolveMetaCredentials), so an
	// account cleared via PUT after the campaign was created does not block pausing or
	// resuming it.
	client := meta.NewClient(meta.Credentials{AccessToken: creds.AccessToken}, meta.AccountConfig{AccountID: strings.TrimSpace(res.accountID), Label: res.label}, d.opts...)
	// Cascade to the ad set (and its ads) as well as the campaign: CreateCampaign PAUSES the
	// campaign, ad set, and every ad, so toggling only the campaign to ACTIVE would not serve.
	// The ad set id is read from the persisted CampaignResult. The ads are DISCOVERED via
	// GET /{adSetID}/ads even though CampaignResult.Ads now records the ones this service
	// created: a live enumeration also covers ads added to the ad set since dispatch, and
	// works for rows written before that field existed.
	// Provenance BEFORE the provisioning guard below, deliberately. A campaign id is unique
	// only within an ad account, so a connection re-pointed since create would address an
	// unrelated campaign — and this path CHANGES delivery, so a collision pauses or activates
	// something this project does not own. "This row has no ad set" is only a meaningful
	// answer once the row is known to belong to the resolved account: on a re-pointed
	// connection the persisted ad set id describes a campaign in a DIFFERENT account, so
	// answering 409-not-provisioned there would explain the wrong campaign. Ordering the two
	// the other way makes a foreign-account ACTIVATE report a missing ad set instead of the
	// mismatch — the trap microsoft.go records at the same seam.
	if err := verifyMetaAccountMatch("toggle meta campaign status", campaign, res.accountID); err != nil {
		return err
	}
	adSetID := metaAdSetID(campaign)
	// ACTIVATE requires a servable tree. A legacy/incomplete "created" row can lack the ad
	// set id (absent/unparseable Result), so activating would fail without ever serving.
	// Refuse before any HTTP call and return ErrCampaignNotProvisioned so the service maps it
	// to a 409 state error (the platform is never contacted), not the default 503 — matching
	// the reddit path. Pausing needs no child id (pausing the parent stops delivery).
	if metaStatus == meta.StatusActive && strings.TrimSpace(adSetID) == "" {
		return fmt.Errorf("%w: meta campaign %s cannot be activated because it has no ad set to serve", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
	}
	if uerr := client.UpdateCampaignAndChildrenStatus(ctx, campaign.PlatformCampaignID, adSetID, metaStatus); uerr != nil {
		// An activate refused up front because the ad set has zero ads is a local/state error
		// (the platform mutation never ran), so classify it as ErrCampaignNotProvisioned → 409,
		// not the default 503 — deterministic "reprovision", not a transient "verify/retry".
		// Mirrors the LinkedIn dispatcher's zero-creatives handling.
		if meta.IsNotServable(uerr) {
			return fmt.Errorf("%w: %s", domain.ErrCampaignNotProvisioned, uerr.Error())
		}
		if meta.IsOutcomeUnconfirmed(uerr) {
			return &unconfirmedToggleError{err: uerr}
		}
		return uerr
	}
	return nil
}

// metaMetricsWindow maps the platform-agnostic model.MetricsWindow vocabulary to Meta's
// own MetricsWindow literals (Insights date_preset values). All seven shared windows are
// supported, so this is a pure rename, not a subset like X Ads' 7-day-capped mapping.
func metaMetricsWindow(w model.MetricsWindow) (meta.MetricsWindow, error) {
	switch w {
	case model.MetricsWindowToday:
		return meta.WindowToday, nil
	case model.MetricsWindowYesterday:
		return meta.WindowYesterday, nil
	case model.MetricsWindowLast7Days:
		return meta.WindowLast7Days, nil
	case model.MetricsWindowLast14Days:
		return meta.WindowLast14Days, nil
	case model.MetricsWindowLast30Days:
		return meta.WindowLast30Days, nil
	case model.MetricsWindowThisMonth:
		return meta.WindowThisMonth, nil
	case model.MetricsWindowLastMonth:
		return meta.WindowLastMonth, nil
	default:
		return "", fmt.Errorf("unsupported metrics window %q", w)
	}
}

// ReadMetrics implements service.MetricsReader for Meta. It resolves the same connection
// ToggleStatus does (no page id or account id required — a metrics read targets the
// campaign node by id via GET /{campaignID}/insights, like the status update; see
// resolveMetaCredentials) and reads the campaign's live Insights metrics, mapping the
// platform-agnostic window to Meta's own vocabulary via metaMetricsWindow before calling
// the client.
func (d *MetaDispatcher) ReadMetrics(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	res, creds, err := d.resolveMetaCredentials(ctx, projectID, platform, d.creds.existingResolver(metaCreationAccountID(campaign)))
	if err != nil {
		return nil, err
	}
	metaWindow, err := metaMetricsWindow(window)
	if err != nil {
		return nil, err
	}
	client := meta.NewClient(meta.Credentials{AccessToken: creds.AccessToken}, meta.AccountConfig{AccountID: strings.TrimSpace(res.accountID), Label: res.label}, d.opts...)
	// Prove the persisted campaign belongs to the account this read is scoped to.
	// resolveMetaCredentials returns the project's CURRENT connection, which can have been
	// re-pointed since create; GET /{campaignID}/insights under a different account yields
	// either a false "no data" or ANOTHER campaign's numbers presented as this campaign's
	// measurement — the failure-as-measurement class this path refuses throughout.
	if err := verifyMetaAccountMatch("read meta campaign metrics", campaign, res.accountID); err != nil {
		return nil, err
	}
	m, err := client.GetCampaignMetrics(ctx, campaign.PlatformCampaignID, metaWindow)
	if err != nil {
		return nil, err
	}
	return &model.CampaignMetrics{
		CampaignID:  m.CampaignID,
		Window:      window,
		Impressions: m.Impressions,
		Clicks:      m.Clicks,
		CostMicros:  m.CostMicros,
		Ctr:         m.Ctr,
		// Conversions is deliberately LEFT NIL, which is a statement about Meta's API rather
		// than an unfinished mapping. The Insights edge exposes no scalar campaign-level
		// conversions field: conversions arrive inside the `actions` array as
		// {action_type, value} objects, and reducing that to one number means deciding which
		// action types count as a conversion for this advertiser — a configuration input this
		// service does not have. Setting 0 here would report every Meta campaign as having
		// converted nothing, which the conversions rule would then flag as a finding.
	}, nil
}

// metaAdSetID pulls the ad set id the create path stored in the persisted CampaignResult
// blob. A missing/unparseable blob yields "" (the campaign is toggled alone — the service
// already blocks toggling a degraded campaign, and on Meta the CAMPAIGN status is the
// effective delivery gate, so a PAUSE without the ad set id still stops serving; only an
// ACTIVATE requires it, and ToggleStatus refuses that up front — see the guard above).
//
// It unmarshals into the SAME meta.CampaignResult type the create path marshals into Result
// (campaignFromMeta), rather than a private struct with a hardcoded "AdSetID" key. This keeps
// ONE definition of the persisted wire shape: if the CampaignResult field/tag is ever renamed,
// reader and writer move together instead of silently desyncing (the previous inline struct
// matched only by coincidence of Go's default field-name marshaling). Making the dependency on
// the create-path result explicit was the intent behind the review note; a dedicated
// model.Campaign.PlatformAdSetID column was considered but is Meta-specific (no other platform
// has an ad set) and would need a schema migration on the shared campaigns table — this keeps
// the fix proportional to a status-toggle PR while removing the fragility.
func metaAdSetID(campaign *model.Campaign) string {
	if campaign == nil || len(campaign.Result) == 0 {
		return ""
	}
	var blob meta.CampaignResult
	if err := json.Unmarshal(campaign.Result, &blob); err != nil {
		return ""
	}
	return blob.AdSetID
}

// metaCreationAccountID reports the ad account the campaign was CREATED under, normalised to
// Meta's documented "act_<digits>" form, or "" when the persisted result blob does not record
// it.
//
// Prefers the explicit AccountID the create path now stamps, and falls back to the act= query
// parameter of the MetaURL the blob has always carried — the create path builds that as
// ".../adsmanager/manage/campaigns?act=" + the account id with its "act_" prefix STRIPPED, so
// the fallback re-adds the prefix to yield the same vocabulary the connection uses. Rows
// written BEFORE the explicit field existed therefore stay checkable rather than silently
// unguarded. Mirrors microsoftCreationAccountID and googleAdsCreationCustomerID.
//
// It unmarshals into the SAME meta.CampaignResult type the create path marshals into Result
// (campaignFromMeta), for the reason metaAdSetID records: one definition of the persisted wire
// shape, so reader and writer move together rather than desyncing.
//
// An EMPTY return means "unknown, proceed": absence must not become a new failure signal for
// pre-existing rows, so only a present-AND-different id is treated as a mismatch by callers.
func metaCreationAccountID(campaign *model.Campaign) string {
	if campaign == nil || len(campaign.Result) == 0 {
		return ""
	}
	var blob meta.CampaignResult
	if err := json.Unmarshal(campaign.Result, &blob); err != nil {
		return ""
	}
	if id := normalizeMetaAccountID(blob.AccountID); id != "" {
		return id
	}
	u, err := url.Parse(blob.MetaURL)
	if err != nil {
		return ""
	}
	return normalizeMetaAccountID(u.Query().Get("act"))
}

// normalizeMetaAccountID puts a Meta ad account id into the single "act_<digits>" vocabulary
// both sides of the provenance comparison must speak. The connection stores the prefixed form
// while MetaURL carries the bare digits, so comparing them raw would report every legacy row
// as a mismatch — a false 409 on a campaign that is perfectly in scope.
//
// Anything that is not a well-formed id normalises to "" — "unknown" — rather than to some
// non-empty token. That is what keeps a malformed value in the guard's "proceed" arm instead
// of letting it act as a REAL account: a bare "act_" (or a stray "act_abc") carries no account,
// but returning it non-empty would compare unequal to every legitimate connection and
// manufacture a false mismatch on a campaign nobody can re-point. Rejecting to "" costs
// nothing, because such a value could never have named an account in the first place.
//
// Meta's documented form is "act_<digits>" (design/connection.go constrains the stored
// connection id to ^act_[0-9]+$), so digits are the whole of the accepted shape and the
// prefix is stripped at most once — "act_act_777" names no account either.
func normalizeMetaAccountID(id string) string {
	digits := strings.TrimPrefix(strings.TrimSpace(id), "act_")
	if digits == "" {
		return ""
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return "act_" + digits
}

// verifyMetaAccountMatch refuses an operation on a campaign that was created under a DIFFERENT
// ad account than the project's current connection resolves to.
//
// Meta campaign ids are unique only WITHIN an ad account, and a project's connection can be
// re-pointed between create and a later read/toggle. Without this check the stored
// PlatformCampaignID is addressed against the NEW account, where it either matches nothing —
// rendered on the read path as a campaign with genuinely zero activity — or collides with an
// unrelated campaign, whose numbers become this campaign's measurement or whose delivery is
// changed by the toggle.
//
// Shared by ReadMetrics and ToggleStatus so the two cannot drift, and returns
// domain.ErrCampaignAccountMismatch exactly as the google-ads and microsoft adapters do.
//
// BOTH sides may be unknown, and neither unknown is a mismatch. An absent CREATED id is the
// pre-existing-row case every adapter documents. An empty CURRENT id is specific to Meta:
// unlike every sibling, toggle and metrics deliberately do NOT require an account selection —
// they address the campaign node by id (POST /{campaignID}, GET /{campaignID}/insights) and
// never read AccountConfig.AccountID, so a connection whose account was cleared via PUT can
// still pause a campaign and read its metrics (see resolveMetaCredentials, and the
// NoAccountIDNeeded tests that pin it). "Not selected" is an ABSENCE, not a different account:
// treating it as one would 409 exactly those paths for any row that records provenance — which,
// via the MetaURL act= fallback, is nearly every historical row — turning a working pause into
// a failure. It would also render the message as "resolves to account " with an empty name.
//
// Takes the account id as a plain string rather than the client the microsoft/reddit/twitter
// siblings accept: those build their client inside a resolve* helper and the caller never holds
// the raw id, so client.AccountID() is their only accessible source. Here — and on linkedin —
// the call site already has res.accountID in hand. normalizeMetaAccountID is applied inside, so
// callers pass the connection value untouched and one place owns the vocabulary.
func verifyMetaAccountMatch(op string, campaign *model.Campaign, accountID string) error {
	created := metaCreationAccountID(campaign)
	current := normalizeMetaAccountID(accountID)
	if created == "" || current == "" || created == current {
		return nil
	}
	return fmt.Errorf("%s: campaign %s was created under meta ad account %s but the project's current connection resolves to account %s: %w",
		op, campaign.PlatformCampaignID, created, current, domain.ErrCampaignAccountMismatch)
}

// metaRunStatus maps the service run state (active/paused) to Meta's status enum.
func metaRunStatus(status string) (string, error) {
	switch status {
	case model.CampaignRunActive:
		return meta.StatusActive, nil
	case model.CampaignRunPaused:
		return meta.StatusPaused, nil
	default:
		return "", fmt.Errorf("unsupported campaign run status %q (want %q or %q)", status, model.CampaignRunActive, model.CampaignRunPaused)
	}
}

// campaignFromMeta maps the client result to the persistence model.
func campaignFromMeta(ctx context.Context, r *meta.CampaignResult, cfg metaConfig) *model.Campaign {
	c := &model.Campaign{
		PlatformCampaignID: r.CampaignID,
		CampaignName:       r.CampaignName,
		Status:             campaignStatusCreated,
	}
	// Persist the budget/schedule/config the caller supplied (Meta honors a
	// lifetime-vs-daily budget flag). ConfigSnapshot captures the validated config,
	// but with every variant's ImageURL SANITIZED: a creative image URL is
	// caller-supplied and may be PRE-SIGNED, whose signature is a bearer credential
	// granting time-boxed read access, and config_snapshot is stored UNENCRYPTED in
	// Postgres. This is the SUCCESS path — it runs on every create with an image, not
	// only on failures — so scrubbing the error sinks alone never covered it. Same
	// reason and same helper as campaignFromReddit's PostURL.
	snapshot := cfg
	if len(cfg.Variants) > 0 {
		// Copy the slice before mutating: cfg is passed by value but Variants shares its
		// backing array with the caller's config, and the FULL url must still reach Meta.
		snapshot.Variants = make([]meta.AdVariant, len(cfg.Variants))
		copy(snapshot.Variants, cfg.Variants)
		for i := range snapshot.Variants {
			snapshot.Variants[i].ImageURL = sanitizeSnapshotURL(snapshot.Variants[i].ImageURL)
		}
	}
	applyCampaignConfig(ctx, c, cfg.Budget, cfg.LifetimeBudget, cfg.StartDate, cfg.EndDate, snapshot)
	if raw, err := json.Marshal(r); err != nil {
		// A marshal failure should be near-impossible for this plain struct, but do NOT
		// swallow it: on the degraded/ambiguous-orphan paths Result is the sole carrier
		// of the per-variant failure Steps and the reconcile-by-name payload, so a
		// silently-empty Result loses reconciliation data precisely when it's most
		// needed. Log it (the row is still persisted with its id/status). Mirrors the
		// linkedin adapter.
		slog.WarnContext(ctx, "failed to marshal meta campaign result blob (Result left empty)",
			"campaign_id", c.PlatformCampaignID, "error", err)
	} else {
		c.Result = raw
	}
	return c
}

// resolveMetaDiscoveryClient builds a Meta client for the ACCOUNT-DISCOVERY path.
//
// The stored-state checks are resolveMetaCredentials' — active status, decodable blob,
// non-empty access token, each tagged with domain.ErrConnectionNotUsable plus its reason
// sentinel and passed through systemScoped. This function deliberately does not repeat
// them. It did once, and that duplication was the defect: the two copies classified the
// same three conditions, so a later change to either — a fourth check, a different sentinel,
// a message that stops dropping the decode cause — would have silently applied to only one
// of "can this connection dispatch?" and "can this connection be asked what it reaches?".
// One source, one error contract, and the discovery endpoint's 400-vs-503 mapping stays
// pinned to the same sentinels the dispatch path answers with.
//
// What IS specific to discovery is what is not required: no account id. That omission is the
// point of the endpoint — it exists to answer "which ad account should this connection
// use?", so demanding one would make it reachable only by connections that no longer need
// it. resolveMetaCredentials never consults the account id (see its godoc for why the
// metrics and toggle paths do not either); Dispatch adds that requirement separately with
// requireMetaAccountID, and this path simply does not call it. That is exactly what makes
// credentials-only bootstrap work: a connection created with an access token and a page id
// but no account_id is usable HERE, which is how its owner discovers the id to PUT.
//
// AccountConfig is left ZERO for the same reason. GET /me/adaccounts is account-agnostic:
// it asks what the TOKEN reaches, so scoping the client to one of the answers would narrow
// the response to a subset of the question.
func (d *MetaDispatcher) resolveMetaDiscoveryClient(ctx context.Context, projectID string, platform model.Provider) (*meta.Client, error) {
	_, creds, err := d.resolveMetaCredentials(ctx, projectID, platform, d.creds.resolve)
	if err != nil {
		return nil, err
	}
	return meta.NewClient(meta.Credentials{AccessToken: creds.AccessToken}, meta.AccountConfig{}, d.opts...), nil
}

// ListAccounts discovers the ad accounts reachable via the project's stored, encrypted
// Meta connection credential, returning minimal identifying information (the act_-prefixed
// account id and a display label).
//
// It satisfies the service-side AccountLister interface, which Orchestrator.ReadAccounts
// type-asserts on the dispatcher for the requested platform; a platform whose dispatcher
// does not implement it gets ErrAccountsUnsupported and the ad platform is never contacted.
// The error contract of resolveMetaDiscoveryClient is what the endpoint's status mapping
// relies on.
func (d *MetaDispatcher) ListAccounts(ctx context.Context, projectID string, platform model.Provider) ([]model.AccessibleAccount, error) {
	client, err := d.resolveMetaDiscoveryClient(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	adAccounts, lerr := client.ListAdAccounts(ctx)
	if lerr != nil {
		return nil, lerr
	}
	// make(..., 0, n) rather than a nil var: a token that legitimately reaches zero ad
	// accounts is an empty list, not an error, and the two must stay distinguishable at
	// the service boundary — Orchestrator.ReadAccounts rejects a nil result as a contract
	// violation precisely so an empty answer keeps its meaning.
	accounts := make([]model.AccessibleAccount, 0, len(adAccounts))
	for _, a := range adAccounts {
		accounts = append(accounts, model.AccessibleAccount{ID: a.ID, Label: metaAccountLabel(a)})
	}
	return accounts, nil
}

// metaAccountLabel builds the string a picker shows for one ad account.
//
// It never returns "" for an account that has any identifying information: an account with
// no `name` falls back to its id, because a blank row in a picker is unpickable and the id
// is what actually gets stored. A KNOWN-BAD account_status is appended in parentheses so
// the user sees WHY the account they were about to choose will be refused by
// CreateCampaign's preflight — which reads the same map — rather than choosing it and
// meeting the refusal one step later, at dispatch, with no way back to this list.
//
// An unrecognized or absent status appends nothing. Meta omits account_status on accounts
// it will not report on, and treating absence as a defect would label a working account.
func metaAccountLabel(a meta.AdAccount) string {
	label := a.Name
	if label == "" {
		label = a.ID
	}
	if reason := a.StatusLabel(); reason != "" {
		label += " (" + reason + ")"
	}
	return label
}
