// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Geo targeting (LFXV2-3279). Microsoft addresses locations by NUMERIC LocationId,
// and this file owns the one mapping between that and the ISO 3166-1 alpha-2 codes the
// sibling dispatchers already speak.
//
// WHY AN INGESTED FILE RATHER THAN A HARDCODED TABLE, verified against Microsoft's own
// v13 reference rather than inherited from this repo's earlier comment:
//
//   - LocationCriterion.LocationId is Add:Required and is the ONLY Add-writable element.
//     DisplayName, LocationType and EnclosedLocationIds are all "Add: Read-only", and the
//     inherited Type is read-only too. A country therefore cannot be named directly —
//     sending DisplayName:"United States" targets nothing. (LocationCriterion reference.)
//   - The geographical-locations file (format version 2.0) has exactly these columns:
//     Location Id, Bing Display Name, Location Type, Replaces, Status, AdWords Location Id.
//     There is NO ISO column, and no documented operation resolves a country code or name
//     to a LocationId. (Geographical Location Codes guide.)
//   - The ISO country-code table in that same guide maps ISO-2 to a country NAME. The guide
//     introduces it with "In some contexts the API requires a country code string e.g., for
//     the business address of an AdvertiserAccount object" — an EXAMPLE, not a statement
//     that the codes are valid nowhere else. So the earlier claim in targeting.go that the
//     table is "explicitly scoped to account business addresses" overstated its source. It
//     does not change the outcome, though: the table yields a NAME, and a name is not a
//     LocationId, so the file is still the only path from "US" to a targetable id.
//
// The resolution is therefore ISO-2 -> country name (Microsoft's own published table, which
// is the authority on how Microsoft spells each country) -> the Location Id of the file row
// whose Location Type is Country and whose Bing Display Name matches that name. Both halves
// come from Microsoft; neither is a LocationId this repo invented, which is the property the
// deferral comment correctly insisted on.
//
// STATUS IS ENFORCED. LocationCriterion's reference says: "Please check the Status field in
// the geographical locations file before adding or updating a location criterion. If the
// status is PendingDeprecation, the location criterion is no longer used for targeting or
// exclusions." A Deprecated or PendingDeprecation row is therefore NOT a usable target, and
// admitting one would recreate the untargeted-campaign harm through the front door. Rows in
// those states are dropped during parse, so an ISO code whose only row is deprecated fails
// resolution rather than silently targeting a dead location.
// ---------------------------------------------------------------------------

const (
	// geoFileVersion is the locations file format this parser understands. Microsoft:
	// "Currently the only supported version is 2.0." The parser is column-NAME driven
	// (see parseGeoLocations), so a future additive revision does not break it, but the
	// version is still pinned rather than left to the server's default.
	geoFileVersion = "2.0"

	// geoFileLanguageLocale selects the language of the Bing Display Name column. This is
	// load-bearing rather than cosmetic: display name is the ONLY join key between an ISO
	// code and a Location Id, and requesting e.g. "de" would return "Vereinigte Staaten"
	// where isoCountryNames says "United States", resolving nothing. The ISO table this
	// client matches against is published in English, so the file must be too.
	geoFileLanguageLocale = "en"

	// geoLocationTypeCountry is the Location Type value identifying a country/region row.
	// Only country rows are retained: this client resolves COUNTRY codes, and a city or
	// postal-code row sharing a display name would otherwise be an ambiguous match.
	geoLocationTypeCountry = "Country"

	// geoCacheTTL bounds how long a parsed locations map is reused before it is re-fetched.
	//
	// SCOPE: the cache lives on the Client, and MicrosoftDispatcher builds a NEW client per
	// Dispatch call, so today this coalesces the fetches WITHIN one campaign create (and any
	// concurrent callers sharing that client) — not across jobs. A cross-job cache would need a
	// longer-lived owner injected into the dispatcher, which is a separate change; claiming one
	// here would be false. The TTL still governs any client that IS retained.
	//
	// Microsoft's own guidance is to "consider calling the GetGeoLocationsFileUrl operation
	// once or twice each month to determine if the contents of the file have changed e.g.,
	// status" — the data moves on the order of months, not minutes. 24h is deliberately far
	// tighter than that: the cost of a stale entry is targeting a location that has since
	// gone PendingDeprecation (money spent on a criterion the delivery engine is retiring),
	// so this errs toward freshness while still collapsing a day of creates onto one fetch.
	geoCacheTTL = 24 * time.Hour

	// maxGeoTargets bounds caller input. Not Microsoft's limit — AddCampaignCriterions
	// accepts "up to 100 campaign criterions per request" — but a sanity cap on this
	// broker's input, mirroring maxKeywords and the google-ads sibling's maxGeoTargets.
	maxGeoTargets = 30

	// maxGeoFileBytes bounds the DECOMPRESSED locations file read. The file is a few MiB of
	// CSV today; this is a generous ceiling that still prevents an unbounded read (or a
	// decompression bomb, since the limit is applied to the DECOMPRESSED stream) from
	// exhausting memory. maxResponseBytes does not apply here: the download is a plain GET
	// against a storage URL, not a doRequest call.
	maxGeoFileBytes = 64 << 20 // 64 MiB

	// maxGeoFileRows bounds the number of retained COUNTRY rows, a second ceiling that is
	// independent of the byte cap: a file within the size limit still must not expand into
	// an unbounded map. Microsoft publishes ~240 countries, so this is ~8x headroom.
	maxGeoFileRows = 2000

	// criterionTypeTargets is the CriterionType sent on AddCampaignCriterions for LOCATION
	// criteria. Microsoft: "To add, delete, or update target criterions i.e., age, day and
	// time, device, gender, location, location intent, and radius criterions, you must
	// specify the CriterionType value as Targets." Sending "Location" here — the value used
	// to READ them back via GetCampaignCriterionsByIds — is rejected on Add.
	criterionTypeTargets = "Targets"

	// criterionTypeLocation is the nested Criterion's JSON TYPE DISCRIMINATOR, used inside the
	// CampaignCriterion body on the ADD path. It is NOT the CriterionType request enum — see
	// readCriterionTypeLocation, which is a different vocabulary with a different spelling.
	criterionTypeLocation = "LocationCriterion"

	// readCriterionTypeLocation is the CriterionType REQUEST ENUM for
	// POST /CampaignCriterions/QueryByIds. It is the bare "Location", not the discriminator
	// "LocationCriterion" and not the ADD path's "Targets": Microsoft documents that Targets
	// "is not allowed for this operation", and that a read asks for one concrete type — the
	// enum members being Age, DayTime, Device, Gender, Location, LocationIntent, Radius.
	//
	// These are deliberately THREE separate constants rather than one reused across the add
	// body, the add request and the read request. Collapsing any two of them produces a call
	// that is silently wrong rather than rejected at compile time: sending the discriminator
	// here made the read return no criteria, which the reuse path would then read as "nothing
	// attached" and re-attach every location, duplicating them.
	readCriterionTypeLocation = "Location"

	// campaignCriterionTypeBiddable is the CampaignCriterion subtype discriminator. Biddable
	// (not Negative) is a POSITIVE target: these are the places the campaign SHOULD serve.
	campaignCriterionTypeBiddable = "BiddableCampaignCriterion"

	// campaignCriterionTypeNegative is the OTHER CampaignCriterion subtype: an EXCLUSION
	// rather than a target. This client never CREATES one, but a campaign it reuses may
	// already carry them, so the reuse read has to recognise the value to avoid counting an
	// exclusion as a satisfied positive target. See existingLocationIDs.
	campaignCriterionTypeNegative = "NegativeCampaignCriterion"
)

// Locations-file column headings, matched case-insensitively by NAME rather than by
// position. Microsoft is explicit that "New columns may be added at any time, so your
// implementation must ignore unknown columns" and that "The order of locations is not
// guaranteed, so you should not take dependencies on any perceived column sort order or
// hierarchy" — so a fixed index would be a latent break on the next additive revision.
const (
	geoColLocationID   = "location id"
	geoColDisplayName  = "bing display name"
	geoColLocationType = "location type"
	geoColStatus       = "status"
)

// errGeoUnresolved marks a geo code this client could not resolve to a LocationId. It is a
// distinct sentinel so CreateCampaign can refuse BEFORE any mutating call while still
// letting the caller tell "your input was bad" apart from "Microsoft was unreachable".
var errGeoUnresolved = errors.New("geo target could not be resolved to a Microsoft location id")

// geoLocations is a parsed, immutable snapshot of the country rows of one locations file.
// It is only ever published whole and never mutated after publication, so a reader holding
// a pointer needs no lock of its own.
type geoLocations struct {
	// byName maps a case-folded country display name to its Location Id.
	byName map[string]string
	// fetchedAt is when this snapshot was parsed, for TTL expiry.
	fetchedAt time.Time
}

// geoLocationsFileURLResponse is the (subset of the) POST /GeoLocationsFileUrl/Query 200 body.
//
// Only FileUrl is decoded. The response also carries FileUrlExpiryTimeUtc and
// LastModifiedTimeUtc, and both are deliberately OMITTED rather than decoded-and-ignored: this
// client re-requests the URL on every refresh (so the expiry is never consulted) and refreshes
// on a TTL rather than conditionally (so the last-modified is never compared). A field that is
// parsed but never read is a claim that something checks it. Microsoft's documented sync
// pattern — store LastModifiedTimeUtc and skip the download when it has not advanced — is a
// worthwhile future optimisation, and that is when the field should be added.
type geoLocationsFileURLResponse struct {
	FileURL string `json:"FileUrl"`
}

// geoLocationsFileURLRequest is the POST body. CompressionType requests GZip, the only
// supported compression format; the download path transparently handles either form, so an
// uncompressed response is still read correctly.
type geoLocationsFileURLRequest struct {
	Version         string `json:"Version"`
	LanguageLocale  string `json:"LanguageLocale"`
	CompressionType string `json:"CompressionType"`
}

// geoCache is the process-local locations cache. It is a struct rather than bare Client
// fields so the single-flight discipline (never hold the mutex across the network call)
// stays visible, mirroring how tokenMu/inflight are documented on Client.
type geoCache struct {
	mu       sync.Mutex
	snapshot *geoLocations
	inflight *geoFetch
}

// geoFetch holds the shared result of one in-flight locations fetch, so N concurrent creates
// trigger ONE multi-MiB download rather than N. Mirrors tokenRefresh.
type geoFetch struct {
	done chan struct{}
	snap *geoLocations
	err  error
}

// validateGeoTargets trims, upper-cases and de-duplicates caller-supplied ISO-2 codes,
// returning them in caller order. It resolves NOTHING — resolution needs the network, and
// this runs in CreateCampaign's pure-validation prologue where a failure is a clean
// (nil, err) with nothing created.
//
// An empty input returns (nil, nil): geo targeting is OPTIONAL at this layer, so a campaign
// created without it behaves exactly as it did before this ticket. Whether an untargeted
// campaign is acceptable is the caller's decision — see CampaignInput.GeoTargets.
//
// An unknown code is a HARD error, not a silent drop, and the asymmetry is the whole point
// of this ticket: dropping "USA" (a plausible typo for "US") would create a campaign that
// spends worldwide while reporting success. This checks membership in Microsoft's OWN
// published ISO table, so a code that is not a Microsoft-supported country is refused here,
// before the network is touched.
func validateGeoTargets(geoTargets []string) ([]string, error) {
	if len(geoTargets) == 0 {
		return nil, nil
	}
	if len(geoTargets) > maxGeoTargets {
		return nil, fmt.Errorf("microsoft-ads: at most %d geo targets are supported, got %d", maxGeoTargets, len(geoTargets))
	}
	seen := make(map[string]struct{}, len(geoTargets))
	out := make([]string, 0, len(geoTargets))
	for _, g := range geoTargets {
		code := strings.ToUpper(strings.TrimSpace(g))
		if code == "" {
			return nil, fmt.Errorf("microsoft-ads: geo target must not be empty")
		}
		if _, ok := isoCountryNames[code]; !ok {
			return nil, fmt.Errorf("microsoft-ads: geo target %q is not a Microsoft-supported ISO 3166-1 alpha-2 country code (e.g. US, GB, DE)",
				truncate(code, maxErrorBodyChars))
		}
		if _, dup := seen[code]; dup {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, code)
	}
	return out, nil
}

// resolveGeoTargets maps validated ISO-2 codes to Microsoft LocationIds, fetching and
// caching the locations file as needed.
//
// It FAILS CLOSED. Every code must resolve; the first that does not aborts with
// errGeoUnresolved and NO ids are returned. Returning the partial set would create a
// campaign targeted at some-but-not-all of the requested countries while reporting success,
// and a caller cannot tell that from a full result. A fetch failure is likewise an error,
// never an empty map treated as "no targeting" — degrading to untargeted is the exact harm
// this ticket exists to prevent.
func (c *Client) resolveGeoTargets(ctx context.Context, codes []string) ([]string, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	locs, err := c.geoLocationsSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(codes))
	for _, code := range codes {
		name, ok := isoCountryNames[code]
		if !ok {
			// validateGeoTargets already rejected unknown codes; this is a defensive
			// restatement so a future caller reaching resolve directly cannot bypass it.
			return nil, fmt.Errorf("microsoft-ads: %w: %q is not a Microsoft-supported country code",
				errGeoUnresolved, truncate(code, maxErrorBodyChars))
		}
		id, ok := locs.byName[strings.ToLower(name)]
		if !ok {
			return nil, fmt.Errorf("microsoft-ads: %w: no active %s row in Microsoft's geographical locations file matches country %q (code %q)",
				errGeoUnresolved, geoLocationTypeCountry, truncate(name, maxErrorBodyChars), truncate(code, maxErrorBodyChars))
		}
		out = append(out, id)
	}
	return out, nil
}

// geoLocationsSnapshot returns a fresh-enough parsed locations map, fetching one if the
// cache is empty or past geoCacheTTL.
//
// Concurrent callers coalesce onto ONE fetch: the locations file is multi-MiB, so N
// concurrent creates each downloading it would be a self-inflicted bandwidth and latency
// problem. The mutex is never held across the network call — it guards only the cache
// read/write and the publication of the inflight pointer — so a slow download cannot
// serialize every create behind it. Mirrors accessTokenValue's leader/follower shape.
func (c *Client) geoLocationsSnapshot(ctx context.Context) (*geoLocations, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("microsoft-ads geo location lookup aborted before any request (context already done): %w", ctxErr)
	}

	c.geo.mu.Lock()
	if s := c.geo.snapshot; s != nil && c.now().Before(s.fetchedAt.Add(geoCacheTTL)) {
		c.geo.mu.Unlock()
		return s, nil
	}
	if f := c.geo.inflight; f != nil {
		c.geo.mu.Unlock()
		// Wait for the leader, but stay cancellable: a follower whose own context is done
		// must not be pinned to a download it no longer needs.
		select {
		case <-f.done:
			return f.snap, f.err
		case <-ctx.Done():
			return nil, fmt.Errorf("microsoft-ads geo location lookup aborted while awaiting the locations file: %w", ctx.Err())
		}
	}
	// Become the leader.
	fetch := &geoFetch{done: make(chan struct{})}
	c.geo.inflight = fetch
	c.geo.mu.Unlock()

	// Run the fetch DETACHED, exactly as accessTokenValue does, and for the same reason: the
	// result is SHARED. Running it on the leader's own ctx meant one caller's cancellation or
	// timeout published that error to every follower — including followers whose own context
	// was still live — so an unrelated create could be failed by a peer's cancel. Since a geo
	// failure aborts CreateCampaign before any mutating call, that turned one client's timeout
	// into other campaigns refusing to create.
	//
	// The goroutine publishes under a DEFER so a panic anywhere in the fetch (JSON, gzip, CSV,
	// or a caller-supplied RoundTripper) still clears inflight and closes done. Without it a
	// panic left inflight set and done never closed, and every later caller waited on that
	// channel forever — a permanent wedge of geo resolution for the process.
	go func() {
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), geoFetchTimeout)
		defer cancel()

		var (
			snap *geoLocations
			err  error
		)
		defer func() {
			if r := recover(); r != nil {
				// Convert the panic into an error for the waiters rather than letting it escape
				// this goroutine and take the process down.
				err = fmt.Errorf("microsoft-ads geo locations fetch panicked: %v", r)
				snap = nil
			}
			c.geo.mu.Lock()
			fetch.snap, fetch.err = snap, err
			if err == nil {
				c.geo.snapshot = snap
			}
			c.geo.inflight = nil
			c.geo.mu.Unlock()
			close(fetch.done)
		}()
		snap, err = c.fetchGeoLocations(fetchCtx)
	}()

	// The leader then waits on the same channel a follower does, so it is cancellable too.
	select {
	case <-fetch.done:
		return fetch.snap, fetch.err
	case <-ctx.Done():
		return nil, fmt.Errorf("microsoft-ads geo location lookup aborted while awaiting the locations file: %w", ctx.Err())
	}
}

// fetchGeoLocations performs the two-step download: resolve a temporary file URL via
// POST /GeoLocationsFileUrl/Query, then GET that URL.
//
// The two steps are deliberately NOT one call. The FileUrl is a short-lived pre-signed
// storage URL — Microsoft says "You might observe that the URL is set to expire 15 minutes
// from the time this operation completes; however, you should not depend on a fixed
// duration" — so it is fetched fresh on every refresh and never cached across the TTL. Only
// the PARSED result is cached.
//
// The Query step is a READ and is marked idempotent so a 429 is retried; it creates nothing,
// so a retry cannot double-create anything.
func (c *Client) fetchGeoLocations(ctx context.Context) (*geoLocations, error) {
	body, err := c.doRequest(ctx, http.MethodPost, "GeoLocationsFileUrl/Query", geoLocationsFileURLRequest{
		Version:         geoFileVersion,
		LanguageLocale:  geoFileLanguageLocale,
		CompressionType: "GZip",
	}, true)
	if err != nil {
		return nil, fmt.Errorf("microsoft-ads geo locations file url lookup failed: %w", err)
	}
	var resp geoLocationsFileURLResponse
	if uErr := json.Unmarshal(body, &resp); uErr != nil {
		return nil, fmt.Errorf("microsoft-ads geo locations file url response could not be decoded: %w", uErr)
	}
	if strings.TrimSpace(resp.FileURL) == "" {
		return nil, errors.New("microsoft-ads geo locations file url response contained no FileUrl")
	}

	raw, err := c.downloadGeoFile(ctx, resp.FileURL)
	if err != nil {
		return nil, err
	}
	byName, err := parseGeoLocations(raw)
	if err != nil {
		return nil, err
	}
	if len(byName) == 0 {
		// A file that parsed cleanly but yielded no usable country is not a valid snapshot:
		// caching it would make every subsequent resolution fail for a day, and treating it
		// as success would be indistinguishable from "no targeting".
		return nil, errors.New("microsoft-ads geo locations file contained no active country rows")
	}
	return &geoLocations{byName: byName, fetchedAt: c.now()}, nil
}

// downloadGeoFile GETs the temporary file URL and returns the DECOMPRESSED bytes.
//
// This does NOT go through doRequest: the FileUrl points at Microsoft's storage, not the
// Campaign Management API, and attaching the developer token / CustomerAccountId headers and
// the OAuth bearer to a pre-signed storage URL would leak credentials to a host that never
// asked for them. The URL carries its own authorization.
//
// The response is read through a limit reader sized at maxGeoFileBytes+1 so an oversized
// file is DETECTED rather than silently truncated into a partial location table — a
// truncated table resolves fewer countries, which fails closed, but reporting it as a clean
// parse would hide a real problem.
func (c *Client) downloadGeoFile(ctx context.Context, fileURL string) ([]byte, error) {
	// The download is a large transfer, so it gets its own budget rather than the per-call
	// msAdsRequestTimeout sized for a JSON API round trip.
	//
	// The CONTEXT deadline alone is not enough, and that was a real defect here: the shared
	// c.httpClient carries Timeout: msAdsRequestTimeout (30s), and http.Client.Timeout covers
	// connect + headers + THE BODY READ, and is NOT extended by a longer context. A multi-MiB
	// CSV on a slow link therefore died at 30s while this code claimed a 3-minute budget —
	// and because resolution is fail-closed, that failed the whole campaign create. So the
	// download runs on a SHALLOW COPY of the client whose Timeout is the download budget,
	// preserving the no-follow redirect policy (a redirect could carry the signed URL
	// elsewhere).
	reqCtx, cancel := context.WithTimeout(ctx, geoDownloadTimeout)
	defer cancel()

	dl := *c.httpClient
	dl.Timeout = geoDownloadTimeout
	dl.CheckRedirect = noFollow

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, fileURL, nil)
	if err != nil {
		// The cause is NOT wrapped: net/url builds a *url.Error carrying the full URL, and this
		// URL is a pre-signed storage link whose query string IS the credential. Mirrors
		// downloadReport in metrics.go, which documents the same reasoning.
		return nil, errors.New("microsoft-ads geo locations file request could not be built")
	}
	// Ask for gzip explicitly AND handle it manually below, because CompressionType:GZip
	// makes the stored object gzip-encoded content rather than a transfer encoding the
	// transport would transparently undo.
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := dl.Do(req)
	if err != nil {
		// The URL can embed a signature; never echo the *url.Error (which carries the full
		// URL) into an error that may be persisted on a campaign step.
		return nil, fmt.Errorf("microsoft-ads geo locations file download failed: %s", safeCause(err))
	}
	// Drain before close so the connection returns to the idle pool: a body closed unread
	// forces the next request to reopen TCP and TLS. Mirrors downloadReport in metrics.go.
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxGeoFileBytes))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Status only — the body of a storage error can echo the signed URL.
		return nil, fmt.Errorf("microsoft-ads geo locations file download returned HTTP %d", resp.StatusCode)
	}

	// Decompress when the payload is gzip, whether that is signalled by the header or by
	// the magic bytes: Microsoft returns gzip CONTENT here, and a server that does not set
	// Content-Encoding would otherwise hand the CSV parser binary.
	peek := bufio.NewReader(io.LimitReader(resp.Body, maxGeoFileBytes+1))
	reader := io.Reader(peek)
	magic, _ := peek.Peek(2)
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") || (len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b) {
		zr, zErr := gzip.NewReader(peek)
		if zErr != nil {
			return nil, fmt.Errorf("microsoft-ads geo locations file could not be decompressed: %w", zErr)
		}
		defer func() { _ = zr.Close() }()
		// Re-apply the cap to the DECOMPRESSED stream: the byte ceiling has to bound what is
		// materialised, not what arrived on the wire, or a small archive could expand without
		// limit.
		reader = io.LimitReader(zr, maxGeoFileBytes+1)
	}

	var buf bytes.Buffer
	if _, rErr := buf.ReadFrom(reader); rErr != nil {
		return nil, fmt.Errorf("microsoft-ads geo locations file could not be read: %s", safeCause(rErr))
	}
	if buf.Len() > maxGeoFileBytes {
		return nil, fmt.Errorf("microsoft-ads geo locations file exceeds the %d byte limit", maxGeoFileBytes)
	}
	return buf.Bytes(), nil
}

// parseGeoLocations parses the locations CSV into a case-folded display-name -> Location Id
// map of ACTIVE country rows.
//
// Columns are located BY NAME from the header row, never by index: Microsoft states that new
// columns may be added at any time and that implementations must ignore unknown ones, so a
// positional parser would break on the next additive revision — and would break by silently
// reading the wrong column, which is the failure mode that puts a wrong LocationId on a paid
// campaign.
//
// Rows whose Status is not Active are DROPPED (see the file header comment): Microsoft
// documents that a PendingDeprecation location "is no longer used for targeting or
// exclusions" and that deprecated criteria cannot be added, so admitting one would produce a
// create that either fails at AddCampaignCriterions or, worse, succeeds and targets nothing.
func parseGeoLocations(raw []byte) (map[string]string, error) {
	r := csv.NewReader(bytes.NewReader(raw))
	// The row width is not fixed across revisions (columns may be added), so do not let the
	// reader enforce the header's field count.
	r.FieldsPerRecord = -1
	// Microsoft's display names contain commas and quoted forms; ReuseRecord keeps the
	// per-row allocation down over ~a hundred thousand rows.
	r.ReuseRecord = true

	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("microsoft-ads geo locations file has no header row: %w", err)
	}
	idx := make(map[string]int, len(header))
	for i, h := range header {
		// Strip a UTF-8 BOM (U+FEFF) from the first heading; the file is UTF-8 and a BOM would make
		// "location id" fail to match, dropping the id column entirely.
		h = strings.TrimPrefix(h, "\ufeff")
		idx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	colID, okID := idx[geoColLocationID]
	colName, okName := idx[geoColDisplayName]
	colType, okType := idx[geoColLocationType]
	colStatus, okStatus := idx[geoColStatus]
	if !okID || !okName || !okType || !okStatus {
		// Refuse rather than parse a file whose shape is not the documented one: guessing at
		// columns here is guessing at which country a paid campaign targets.
		return nil, fmt.Errorf("microsoft-ads geo locations file is missing required columns (need %q, %q, %q, %q)",
			geoColLocationID, geoColDisplayName, geoColLocationType, geoColStatus)
	}

	out := make(map[string]string)
	// Names seen on two or more DISTINCT Active Country rows. They are removed below rather
	// than resolved arbitrarily.
	ambiguous := make(map[string]struct{})
	for {
		rec, rErr := r.Read()
		if errors.Is(rErr, io.EOF) {
			break
		}
		if rErr != nil {
			return nil, fmt.Errorf("microsoft-ads geo locations file could not be parsed: %w", rErr)
		}
		if colID >= len(rec) || colName >= len(rec) || colType >= len(rec) || colStatus >= len(rec) {
			// A short row cannot be classified; skip it rather than index out of range. It
			// simply contributes no target, which fails closed for that country.
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(rec[colType]), geoLocationTypeCountry) {
			continue
		}
		// Status is compared with spaces stripped so "Pending Deprecation" and
		// "PendingDeprecation" — the guide uses the former in the table and the latter in the
		// LocationCriterion reference — are both recognised as NOT active.
		if !strings.EqualFold(collapseSpace(rec[colStatus]), "active") {
			continue
		}
		id := strings.TrimSpace(rec[colID])
		if !idRE.MatchString(id) {
			// Reuse the client's positive-integer id rule: a non-numeric or zero/negative id
			// is not a usable LocationId and must not enter the map.
			continue
		}
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(rec[colName]))
		if name == "" {
			continue
		}
		if prev, dup := out[name]; dup {
			// AMBIGUOUS, not first-wins. Microsoft warns that "Multiple location IDs can have
			// the same display name" and that row order is not guaranteed — so picking the
			// first is arbitrary ACROSS REFRESHES, not merely within one file: the same brief
			// could silently resolve to a different LocationId tomorrow. A name with two Active
			// Country rows is therefore recorded as unusable, and resolving it REFUSES. Failing
			// closed on an ambiguous name beats spending money on an arbitrary one.
			if prev != id {
				ambiguous[name] = struct{}{}
			}
			continue
		}
		if len(out) >= maxGeoFileRows {
			// REFUSE rather than break. A silent truncation returns a partial map that parses
			// cleanly, gets cached for geoCacheTTL, and then fails every create whose country
			// happened to sit past the cap — with an error blaming the operator's geo code.
			// The byte cap is deliberately sized +1 so an overflow is DETECTED; this must be
			// detectable for the same reason.
			return nil, fmt.Errorf("microsoft-ads geo locations file contains more than %d active country rows, which exceeds this client's limit", maxGeoFileRows)
		}
		out[name] = id
	}
	for name := range ambiguous {
		delete(out, name)
	}
	return out, nil
}

// collapseSpace removes all whitespace from s, used to compare Status values whose spelling
// varies between Microsoft's own documents ("Pending Deprecation" vs "PendingDeprecation").
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// ---------------------------------------------------------------------------
// Attaching location criteria (POST /CampaignCriterions).
//
// Location criteria go at the CAMPAIGN level. The wire contract differs from every other
// create in this client in two ways that a copy of the sibling shape would get wrong:
//
//   - CriterionType MUST be "Targets", not "Location". Microsoft: "To add, delete, or update
//     target criterions i.e., age, day and time, device, gender, location, location intent,
//     and radius criterions, you must specify the CriterionType value as Targets." "Location"
//     is a value you request on the READ path (GetCampaignCriterionsByIds), not on Add.
//   - The response carries NestedPartialErrors — an array of BatchErrorCollection, each with
//     its OWN nested BatchErrors array — NOT the flat PartialErrors every other create here
//     returns. A flat decode would silently see zero errors and report a rejected criterion
//     as success, which is precisely an untargeted campaign reported as targeted.
//
// Each criterion is a BiddableCampaignCriterion whose nested Criterion is a LocationCriterion
// carrying only LocationId; the other elements are Add:Read-only. CriterionBid is OMITTED:
// this attaches plain targeting, and a bid multiplier is a separate product decision that
// would change what the campaign pays.
//
// Microsoft auto-creates a LocationIntentCriterion with the default
// PeopleInOrSearchingForOrViewingPages when a campaign's first criterion is added, so this
// client sends none — inventing an intent option would silently change who sees the ads.
// ---------------------------------------------------------------------------

// msLocationCriterion is the nested Criterion of a campaign criterion. Type is the
// discriminator; LocationId is the only Add-writable element.
type msLocationCriterion struct {
	Type       string      `json:"Type"`
	LocationId json.Number `json:"LocationId"`
}

// msCampaignCriterion is one entry in the CampaignCriterions array. Type discriminates the
// CampaignCriterion subtype (Biddable vs Negative) and is required on the JSON wire form —
// unlike the SDKs, where the object's class carries it.
type msCampaignCriterion struct {
	CampaignId json.Number         `json:"CampaignId"`
	Criterion  msLocationCriterion `json:"Criterion"`
	Type       string              `json:"Type"`
}

// createCampaignCriterionsRequest is the POST /CampaignCriterions body.
type createCampaignCriterionsRequest struct {
	CampaignCriterions []msCampaignCriterion `json:"CampaignCriterions"`
	CriterionType      string                `json:"CriterionType"`
}

// msNestedErrorCollection is one BatchErrorCollection: a top-level error plus its own nested
// BatchErrors. Both levels are decoded because either can carry the reason a criterion was
// rejected — the top level for criterion-level failures, the nested list for item-level ones.
type msNestedErrorCollection struct {
	Code        json.RawMessage   `json:"Code"`
	ErrorCode   json.RawMessage   `json:"ErrorCode"`
	BatchErrors boundedErrorItems `json:"BatchErrors"`
}

// createCampaignCriterionsResponse is the (subset of the) 200 body.
type createCampaignCriterionsResponse struct {
	CampaignCriterionIds boundedGeoIDs           `json:"CampaignCriterionIds"`
	NestedPartialErrors  boundedNestedErrorItems `json:"NestedPartialErrors"`
}

// boundedGeoIDs is boundedNumberIDs bounded by maxGeoTargets. Every id matters here (one per
// requested country), so the 16-item error-array bound would silently truncate a 30-country
// request — the same defect boundedKeywordIDs exists to avoid.
type boundedGeoIDs []*json.Number

func (b *boundedGeoIDs) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil { // JSON null
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("expected a JSON array for campaign criterion ids")
	}
	for dec.More() {
		var n *json.Number
		if err := dec.Decode(&n); err != nil {
			return err
		}
		if len(*b) < maxGeoTargets {
			*b = append(*b, n)
		}
	}
	return nil
}

// boundedNestedErrorItems bounds the NestedPartialErrors array the same way boundedErrorItems
// bounds a flat one, and tracks truncation for the same reason.
type boundedNestedErrorItems struct {
	Items     []msNestedErrorCollection
	Truncated bool
}

func (b *boundedNestedErrorItems) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil { // JSON null
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("expected a JSON array for nested error items")
	}
	for dec.More() {
		var it msNestedErrorCollection
		if err := dec.Decode(&it); err != nil {
			return err
		}
		if len(b.Items) < maxDecodedErrorItems {
			b.Items = append(b.Items, it)
			continue
		}
		b.Truncated = true
	}
	return nil
}

// nestedErrorsHaveAny reports whether the collection carries at least one ACTUAL error,
// checking BOTH the collection level and each nested BatchErrors list. Mirrors
// partialErrorsHaveAny's null-placeholder filtering: a position-aligned array can contain
// nulls that decode to zero-value items, and those are not errors.
func nestedErrorsHaveAny(items []msNestedErrorCollection) bool {
	for _, it := range items {
		if codeString(it.ErrorCode) != "" || codeString(it.Code) != "" {
			return true
		}
		if partialErrorsHaveAny(it.BatchErrors.Items) {
			return true
		}
	}
	return false
}

// nestedErrorCodes renders the machine-readable codes from a NestedPartialErrors array for an
// error message. Only codes are surfaced (never Message/Details, which can echo account or
// entity specifics), matching the apiError contract that governs every other error string in
// this client.
func nestedErrorCodes(items []msNestedErrorCollection) string {
	var codes []string
	for _, it := range items {
		for _, raw := range []json.RawMessage{it.ErrorCode, it.Code} {
			if v := codeString(raw); v != "" && len(v) <= maxErrorCodeLen {
				codes = append(codes, v)
				if len(codes) >= maxRetainedErrorCodes {
					return strings.Join(codes, ",")
				}
			}
		}
		if nested := partialErrorCodes(it.BatchErrors.Items); nested != "" && nested != "unspecified" {
			codes = append(codes, nested)
			if len(codes) >= maxRetainedErrorCodes {
				return strings.Join(codes, ",")
			}
		}
	}
	if len(codes) == 0 {
		return "unspecified"
	}
	return strings.Join(codes, ",")
}

// createCampaignCriterions attaches every resolved LocationId to the campaign as a SINGLE
// AddCampaignCriterions call, returning the created criterion ids in request order.
//
// Batched into one call rather than one per country so the whole geo set shares ONE outcome:
// N calls would leave a PARTIALLY TARGETED campaign on any mid-sequence failure — a campaign
// targeting the US when the caller asked for US+GB+DE, which spends real money in the wrong
// shape while every individual call reported success.
//
// NOT RETRIED ON 429, like every other mutating create here: the call has no idempotency key,
// so a retry after a throttled-but-committed request would attach a second copy of each
// criterion.
func (c *Client) createCampaignCriterions(ctx context.Context, campaignID string, locationIDs []string) ([]string, error) {
	if len(locationIDs) == 0 {
		return nil, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("microsoft-ads geo targeting aborted before any request (context already done; campaign %s has no location criteria yet): %w", campaignID, ctxErr)
	}

	criterions := make([]msCampaignCriterion, 0, len(locationIDs))
	for _, id := range locationIDs {
		criterions = append(criterions, msCampaignCriterion{
			CampaignId: json.Number(campaignID),
			Criterion: msLocationCriterion{
				Type:       criterionTypeLocation,
				LocationId: json.Number(id),
			},
			Type: campaignCriterionTypeBiddable,
		})
	}

	body, err := c.doRequest(ctx, http.MethodPost, "CampaignCriterions", createCampaignCriterionsRequest{
		CampaignCriterions: criterions,
		CriterionType:      criterionTypeTargets,
	}, false)
	if err != nil {
		return nil, err
	}

	var resp createCampaignCriterionsResponse
	if uErr := json.Unmarshal(body, &resp); uErr != nil {
		// A 2xx whose body will not parse is UNCONFIRMED: the criteria may have been attached
		// and a blind retry would duplicate them. errNoID is the sentinel the call site maps to
		// UNCONFIRMED.
		return nil, fmt.Errorf("decode CampaignCriterionIds response (%v): %w", uErr, errNoID)
	}

	// A TRUNCATED error array is NOT evidence of success. boundedNestedErrorItems retains
	// maxDecodedErrorItems entries while this call sends up to maxGeoTargets, so a rejection
	// past the cap was DISCARDED during decode and the surviving prefix can read as clean.
	// Absence of a rejection in a truncated array is not absence of a rejection — refusing
	// beats reporting an untargeted campaign as targeted. (Same invariant the keyword path
	// applies via PartialErrors.Truncated.)
	if resp.NestedPartialErrors.Truncated || nestedTruncated(resp.NestedPartialErrors.Items) {
		return nil, fmt.Errorf("microsoft-ads geo targeting returned a truncated error array for campaign %s, so the outcome cannot be classified: %w", campaignID, errNoID)
	}
	if nestedErrorsHaveAny(resp.NestedPartialErrors.Items) {
		created := make([]string, 0, len(resp.CampaignCriterionIds))
		for _, raw := range resp.CampaignCriterionIds {
			if id := numberID(raw); id != "" {
				created = append(created, id)
			}
		}
		// ANY rejected location criterion fails the whole step. Unlike keywords — where a
		// duplicate is Microsoft confirming the desired state already holds — a rejected
		// LOCATION leaves the campaign targeting a SMALLER area than asked for, and a campaign
		// that serves in fewer countries than intended is still a campaign spending money on a
		// targeting shape nobody approved. There is no benign rejection here to special-case.
		if len(created) == 0 {
			return nil, fmt.Errorf("%w: %s", errPartialFailure, nestedErrorCodes(resp.NestedPartialErrors.Items))
		}
		return created, fmt.Errorf("%w (%d of %d location criteria were created): %s",
			errPartialFailure, len(created), len(criterions), nestedErrorCodes(resp.NestedPartialErrors.Items))
	}

	// The id array is index-aligned with the request. A SHORT array means the response does
	// not describe what was sent, so which criteria exist upstream is unknown → UNCONFIRMED.
	want := len(criterions)
	if len(resp.CampaignCriterionIds) < want {
		return nil, fmt.Errorf("microsoft-ads geo targeting returned %d ids for %d location criteria (campaign %s): %w",
			len(resp.CampaignCriterionIds), want, campaignID, errNoID)
	}

	ids := make([]string, 0, want)
	for i := 0; i < want; i++ {
		id := numberID(resp.CampaignCriterionIds[i])
		if id == "" {
			// A null/malformed id slot with NO error explaining it is the malformed-200 case:
			// the criterion may exist but cannot be identified → UNCONFIRMED.
			return nil, fmt.Errorf("microsoft-ads geo targeting returned no usable id at index %d (campaign %s): %w", i, campaignID, errNoID)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// ---------------------------------------------------------------------------
// Reading existing location criteria (POST /CampaignCriterions/QueryByIds).
//
// This exists because neither guess about a REUSED campaign is safe. Re-posting blindly
// duplicates every location (AddCampaignCriterions publishes no duplicate refusal, unlike
// AddKeywords' 1517/1542). Skipping blindly reports success for a campaign whose earlier attach
// was REJECTED — an untargeted campaign that serves everywhere, the exact harm this ticket
// exists to prevent. Only a READ distinguishes "already targeted" from "never targeted".
//
// Two wire details differ from the ADD path and are easy to get wrong:
//   - CriterionType is "Location" here, NOT "Targets". Microsoft: "The Targets value is not
//     allowed for this operation." Targets is the ADD-side grouping; the read asks for one
//     concrete type.
//   - CampaignCriterionIds is sent as NULL to mean "all of them": "If this element is null,
//     all criterions for the specified CampaignId will be retrieved." That is the only way to
//     enumerate criteria whose ids this run never learned.
//
// PartialErrors here is the FLAT BatchError array (like most reads), not the nested
// BatchErrorCollection the ADD path returns.
// ---------------------------------------------------------------------------

// queryCampaignCriterionsRequest is the POST /CampaignCriterions/QueryByIds body.
// CampaignCriterionIds is a nil slice rendered as JSON null — see the block comment.
type queryCampaignCriterionsRequest struct {
	CampaignCriterionIds []json.Number `json:"CampaignCriterionIds"`
	CampaignId           json.Number   `json:"CampaignId"`
	CriterionType        string        `json:"CriterionType"`
}

// queryCampaignCriterionsResponse is the (subset of the) 200 body.
//
// The OUTER Type is decoded, not just the nested Criterion's. A CampaignCriterion is a
// polymorphic wrapper whose Type is either BiddableCampaignCriterion (a POSITIVE target) or
// NegativeCampaignCriterion (an EXCLUSION), and the add path sets it explicitly
// (campaignCriterionTypeBiddable). Reading only the nested LocationId conflates the two: an
// existing US EXCLUSION would satisfy a requested US TARGET, the attach would be skipped, and
// the campaign would be left excluding the very country it was asked to serve — while this run
// reported the targeting as already present. The wrapper Type is the only field that
// distinguishes them, so it has to survive the decode.
//
// It is a POINTER so an ABSENT key is distinguishable from an empty string: absence means the
// response does not describe the criterion's polarity, which is not evidence that it is
// positive. See existingLocationIDs, which fails closed on both absence and any unrecognised
// value rather than guessing.
type queryCampaignCriterionsResponse struct {
	CampaignCriterions []struct {
		Type      *string `json:"Type"`
		Criterion struct {
			Type       string       `json:"Type"`
			LocationId *json.Number `json:"LocationId"`
		} `json:"Criterion"`
	} `json:"CampaignCriterions"`
	PartialErrors boundedErrorItems `json:"PartialErrors"`
}

// existingLocationIDs returns the set of LocationIds already attached to the campaign.
//
// A READ, so it is retried on 429. Any failure is propagated rather than being reported as an
// empty set: "we could not check" must never collapse into "there is no targeting", which would
// send the caller down the re-attach path and duplicate every criterion.
func (c *Client) existingLocationIDs(ctx context.Context, campaignID string) (map[string]struct{}, error) {
	body, err := c.doRequest(ctx, http.MethodPost, "CampaignCriterions/QueryByIds", queryCampaignCriterionsRequest{
		CampaignCriterionIds: nil, // null => every criterion of this type on the campaign
		CampaignId:           json.Number(campaignID),
		CriterionType:        readCriterionTypeLocation,
	}, true)
	if err != nil {
		return nil, err
	}
	var resp queryCampaignCriterionsResponse
	if uErr := json.Unmarshal(body, &resp); uErr != nil {
		return nil, fmt.Errorf("decode CampaignCriterions/QueryByIds response: %w", uErr)
	}
	// A TRUNCATED error array cannot be read as "no errors" here for the same reason it cannot
	// on the add path: an error past the decode cap was DISCARDED, so a clean-looking prefix is
	// not evidence the read succeeded. Under-reporting the existing criteria sends the reuse
	// path into re-attaching locations that are already there.
	if resp.PartialErrors.Truncated {
		return nil, errors.New("microsoft-ads location criterion read returned a truncated error array, so the campaign's existing targeting cannot be determined")
	}
	if partialErrorsHaveAny(resp.PartialErrors.Items) {
		// A read that partly failed does not describe the campaign's targeting, so it cannot be
		// used to decide whether to attach.
		return nil, fmt.Errorf("microsoft-ads location criterion read reported errors: %s", partialErrorCodes(resp.PartialErrors.Items))
	}
	out := make(map[string]struct{}, len(resp.CampaignCriterions))
	for _, cc := range resp.CampaignCriterions {
		id := numberID(cc.Criterion.LocationId)
		if id == "" {
			continue
		}
		// ONLY a Biddable criterion is a positive target. A NegativeCampaignCriterion carrying
		// the same LocationId is an EXCLUSION of that country, and counting it as "already
		// targeted" would skip the attach and leave the campaign excluding the country it was
		// asked to serve — worse than the duplicate this read exists to prevent, because it is
		// silent and the steps line would claim the targeting is present.
		//
		// FAIL CLOSED on anything else, including an ABSENT Type. An unrecognised or missing
		// wrapper type means this read cannot classify the criterion's polarity, and "we could
		// not classify it" must not collapse into either "it is a target" (skips a needed
		// attach) or "it is not" (duplicates a criterion that is already there). Both wrong
		// answers spend money, so the read refuses instead — the same discipline the truncation
		// and PartialErrors guards above apply.
		if cc.Type == nil {
			return nil, fmt.Errorf("microsoft-ads location criterion read returned a criterion (location %s) with no Type, so it cannot be told apart from an exclusion and the campaign's existing targeting cannot be determined", id)
		}
		if *cc.Type == campaignCriterionTypeNegative {
			// A known exclusion is classified, not an error — it simply is not a positive
			// target, so it does not enter the set and the location is attached as requested.
			continue
		}
		if *cc.Type != campaignCriterionTypeBiddable {
			return nil, fmt.Errorf("microsoft-ads location criterion read returned an unrecognised criterion type %q (location %s), so the campaign's existing targeting cannot be determined",
				truncate(*cc.Type, maxErrorBodyChars), id)
		}
		out[id] = struct{}{}
	}
	return out, nil
}

// nestedTruncated reports whether any collection's own BatchErrors array was truncated during
// decode. The outer array has its own flag; this covers the inner ones, which are bounded
// independently and can hide a rejection just as effectively.
func nestedTruncated(items []msNestedErrorCollection) bool {
	for _, it := range items {
		if it.BatchErrors.Truncated {
			return true
		}
	}
	return false
}
