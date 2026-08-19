// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// geoFileHeader is the version 2.0 header row, transcribed from Microsoft's published
// "File Format Version 2.0" column table in the Geographical Location Codes guide, in the
// documented order:
//
//	Location Id | Bing Display Name | Location Type | Replaces | Status | AdWords Location Id
//
// It is written out here rather than generated from this package's own column constants on
// purpose. A fixture built from the parser's own idea of the format cannot falsify that idea:
// if the constants were wrong, a derived fixture would be wrong the same way and every test
// would still pass. This string is the vendor's, so a drift between it and the parser is a
// real failure.
const geoFileHeader = "Location Id,Bing Display Name,Location Type,Replaces,Status,AdWords Location Id"

// geoFileFixture builds a locations CSV with the documented header and the supplied rows.
func geoFileFixture(rows ...string) string {
	return strings.Join(append([]string{geoFileHeader}, rows...), "\n") + "\n"
}

// Representative rows in the documented column order. The display names are Microsoft's own
// spellings, matching the Country Codes table that isoCountryNames transcribes.
var (
	geoRowUS = "190,United States,Country,,Active,2840"
	geoRowGB = "200,United Kingdom,Country,,Active,2826"
	geoRowJP = "182,Japan,Country,,Active,2392"
	geoRowDE = "168,Germany,Country,,Active,2276"
	// A city row that must NOT satisfy a country lookup even though its display name leads
	// with a country component.
	geoRowSeattleCity = "79324,Seattle|Washington|United States,City,,Active,1027744"
)

// geoAPI is a scriptable Microsoft API + locations-file host for the geo tests. It records
// every path it is asked for, which is what lets a test assert that NO mutating call was
// issued rather than merely that an error came back.
type geoAPI struct {
	mu    sync.Mutex
	paths []string

	// fileBody is the CSV served at the download URL.
	fileBody string
	// gzipFile serves fileBody gzip-compressed, as CompressionType:GZip requests.
	gzipFile bool
	// fileStatus overrides the download response status.
	fileStatus int
	// urlQueryStatus / urlQueryBody script POST /GeoLocationsFileUrl/Query.
	urlQueryStatus int
	urlQueryBody   string
	// criterionBody / criterionStatus script POST /CampaignCriterions.
	criterionBody   string
	criterionStatus int
	criterionSeen   *createCampaignCriterionsRequest
	// fileServerURL is filled in by newGeoClient.
	fileServerURL string
	// downloads counts locations-file downloads, for the cache/coalescing tests.
	downloads atomic.Int32
}

func (g *geoAPI) record(p string) {
	g.mu.Lock()
	g.paths = append(g.paths, p)
	g.mu.Unlock()
}

// sawPath reports whether any request hit a path with the given suffix.
func (g *geoAPI) sawPath(suffix string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.paths {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}

// mutatingPaths returns every recorded path that CREATES something upstream. The QueryBy…
// reads and the file lookup are excluded: they are reads, and a test asserting "nothing was
// created" must not trip over them.
func (g *geoAPI) mutatingPaths() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, p := range g.paths {
		if strings.Contains(p, "/QueryBy") || strings.HasSuffix(p, "/GeoLocationsFileUrl/Query") {
			continue
		}
		switch {
		case strings.HasSuffix(p, "/Campaigns"),
			strings.HasSuffix(p, "/AdGroups"),
			strings.HasSuffix(p, "/Ads"),
			strings.HasSuffix(p, "/Keywords"),
			strings.HasSuffix(p, "/CampaignCriterions"):
			out = append(out, p)
		}
	}
	return out
}

func (g *geoAPI) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		g.record(p)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/GeoLocationsFileUrl/Query"):
			if g.urlQueryStatus != 0 {
				w.WriteHeader(g.urlQueryStatus)
			}
			if g.urlQueryBody != "" {
				_, _ = io.WriteString(w, g.urlQueryBody)
				return
			}
			_, _ = io.WriteString(w, fmt.Sprintf(
				`{"FileUrl":%q,"FileUrlExpiryTimeUtc":"2026-08-18T12:15:00Z","LastModifiedTimeUtc":"2026-06-05T18:43:00Z"}`,
				g.fileServerURL))
		case strings.HasSuffix(p, "/CampaignCriterions"):
			decodeTo(t, r, g.criterionSeen)
			writeStatusOr(w, g.criterionStatus, g.criterionBody, `{"CampaignCriterionIds":[9001],"NestedPartialErrors":[]}`)
		case strings.HasSuffix(p, "/Campaigns/QueryByAccountId"):
			_, _ = io.WriteString(w, `{"Campaigns":[]}`)
		case strings.HasSuffix(p, "/AdGroups/QueryByCampaignId"):
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case strings.HasSuffix(p, "/Ads/QueryByAdGroupId"):
			_, _ = io.WriteString(w, `{"Ads":[]}`)
		case strings.HasSuffix(p, "/Campaigns"):
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/AdGroups"):
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/Ads"):
			_, _ = io.WriteString(w, `{"AdIds":[987],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/Keywords"):
			_, _ = io.WriteString(w, `{"KeywordIds":[701,702,703],"PartialErrors":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
}

// newGeoClient wires a client against the geo API plus a separate host serving the locations
// file, mirroring production where the FileUrl points at storage rather than the API.
func newGeoClient(t *testing.T, g *geoAPI) *Client {
	t.Helper()
	fileSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		g.downloads.Add(1)
		if g.fileStatus != 0 {
			w.WriteHeader(g.fileStatus)
			return
		}
		if g.gzipFile {
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			_, _ = zw.Write([]byte(g.fileBody))
			_ = zw.Close()
			return
		}
		_, _ = io.WriteString(w, g.fileBody)
	}))
	t.Cleanup(fileSrv.Close)
	g.fileServerURL = fileSrv.URL + "/geolocations.csv"
	return newAPIClient(t, g.handler(t))
}

// ---- parsing ---------------------------------------------------------------

func TestParseGeoLocations_ReadsCountryRowsFromThePublishedFormat(t *testing.T) {
	got, err := parseGeoLocations([]byte(geoFileFixture(geoRowUS, geoRowGB, geoRowJP)))
	if err != nil {
		t.Fatalf("parseGeoLocations: %v", err)
	}
	want := map[string]string{
		"united states":  "190",
		"united kingdom": "200",
		"japan":          "182",
	}
	for name, id := range want {
		if got[name] != id {
			t.Errorf("byName[%q] = %q, want %q", name, got[name], id)
		}
	}
	if len(got) != len(want) {
		t.Errorf("parsed %d rows, want %d: %v", len(got), len(want), got)
	}
}

// The parser must locate columns BY NAME. Microsoft states new columns may be added at any
// time and that implementations must ignore unknown ones, so a reordered/extended header is
// a valid file — and a positional parser would read the wrong column, which is how a wrong
// LocationId reaches a paid campaign.
func TestParseGeoLocations_LocatesColumnsByNameNotPosition(t *testing.T) {
	reordered := "AdWords Location Id,Status,Bing Display Name,Some Future Column,Location Type,Replaces,Location Id\n" +
		"2840,Active,United States,whatever,Country,,190\n"
	got, err := parseGeoLocations([]byte(reordered))
	if err != nil {
		t.Fatalf("parseGeoLocations: %v", err)
	}
	if got["united states"] != "190" {
		t.Fatalf("byName[united states] = %q, want %q (columns must be located by NAME)", got["united states"], "190")
	}
}

// Status enforcement is a money guard, not hygiene: Microsoft documents that a
// PendingDeprecation location "is no longer used for targeting or exclusions", so admitting
// one produces a campaign whose targeting silently does nothing.
func TestParseGeoLocations_DropsNonActiveRows(t *testing.T) {
	for _, status := range []string{"Pending Deprecation", "PendingDeprecation", "Deprecated"} {
		t.Run(status, func(t *testing.T) {
			row := "190,United States,Country," + "," + status + ",2840"
			got, err := parseGeoLocations([]byte(geoFileFixture(row)))
			if err != nil {
				t.Fatalf("parseGeoLocations: %v", err)
			}
			if _, ok := got["united states"]; ok {
				t.Fatalf("status %q was admitted as targetable; it must be dropped", status)
			}
		})
	}
}

func TestParseGeoLocations_IgnoresNonCountryRows(t *testing.T) {
	got, err := parseGeoLocations([]byte(geoFileFixture(geoRowSeattleCity)))
	if err != nil {
		t.Fatalf("parseGeoLocations: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("non-country rows must not enter the country map, got %v", got)
	}
}

func TestParseGeoLocations_RejectsFileMissingRequiredColumns(t *testing.T) {
	// A header with no "Location Id" column: the parser must refuse rather than guess.
	bad := "Bing Display Name,Location Type,Status\nUnited States,Country,Active\n"
	if _, err := parseGeoLocations([]byte(bad)); err == nil {
		t.Fatal("expected a missing-column error, got nil")
	}
}

// ---- validation (offline, pre-network) -------------------------------------

func TestValidateGeoTargets_NormalisesAndDedupes(t *testing.T) {
	got, err := validateGeoTargets([]string{" us ", "Us", "gb"})
	if err != nil {
		t.Fatalf("validateGeoTargets: %v", err)
	}
	want := []string{"US", "GB"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestValidateGeoTargets_RejectsUnsupportedCode(t *testing.T) {
	for _, code := range []string{"USA", "XX", "  ", "U"} {
		if _, err := validateGeoTargets([]string{code}); err == nil {
			t.Errorf("code %q must be refused, not dropped", code)
		}
	}
}

func TestValidateGeoTargets_EmptyIsNoError(t *testing.T) {
	got, err := validateGeoTargets(nil)
	if err != nil || got != nil {
		t.Fatalf("empty input = (%v, %v), want (nil, nil)", got, err)
	}
}

// ---- resolution ------------------------------------------------------------

func TestResolveGeoTargets_MapsISOCodesToLocationIds(t *testing.T) {
	g := &geoAPI{fileBody: geoFileFixture(geoRowUS, geoRowGB, geoRowJP)}
	c := newGeoClient(t, g)
	got, err := c.resolveGeoTargets(context.Background(), []string{"US", "JP"})
	if err != nil {
		t.Fatalf("resolveGeoTargets: %v", err)
	}
	want := []string{"190", "182"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Order is the CALLER's, not the file's. Microsoft states the order of locations in the file
// is not guaranteed, so a resolver that returned ids in file order would silently reorder the
// caller's targeting — harmless for a set, but it would mean the resolver is reading the file
// rather than the request.
func TestResolveGeoTargets_PreservesCallerOrderAcrossManyCountries(t *testing.T) {
	// File order deliberately differs from the requested order.
	g := &geoAPI{fileBody: geoFileFixture(geoRowJP, geoRowDE, geoRowUS, geoRowGB)}
	c := newGeoClient(t, g)
	got, err := c.resolveGeoTargets(context.Background(), []string{"GB", "US", "DE", "JP"})
	if err != nil {
		t.Fatalf("resolveGeoTargets: %v", err)
	}
	want := []string{"200", "190", "168", "182"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (caller order must be preserved)", got, want)
		}
	}
}

func TestResolveGeoTargets_ReadsGzippedFile(t *testing.T) {
	g := &geoAPI{fileBody: geoFileFixture(geoRowUS), gzipFile: true}
	c := newGeoClient(t, g)
	got, err := c.resolveGeoTargets(context.Background(), []string{"US"})
	if err != nil {
		t.Fatalf("resolveGeoTargets on a gzip file: %v", err)
	}
	if len(got) != 1 || got[0] != "190" {
		t.Fatalf("got %v, want [190]", got)
	}
}

// A code whose only row is non-Active must FAIL, not resolve. This is the parse-level guard
// observed end to end: the country is in the file, so a status-blind parser would resolve it.
func TestResolveGeoTargets_RefusesDeprecatedCountry(t *testing.T) {
	g := &geoAPI{fileBody: geoFileFixture("190,United States,Country,,Deprecated,2840", geoRowGB)}
	c := newGeoClient(t, g)
	_, err := c.resolveGeoTargets(context.Background(), []string{"US"})
	if err == nil {
		t.Fatal("a deprecated location must not resolve")
	}
	if !errors.Is(err, errGeoUnresolved) {
		t.Fatalf("error = %v, want errGeoUnresolved", err)
	}
}

// Partial resolution must fail WHOLE. Returning the ids that did resolve would target the
// campaign at a subset of the requested countries while reporting success.
func TestResolveGeoTargets_FailsWholeWhenOneCodeIsUnresolvable(t *testing.T) {
	g := &geoAPI{fileBody: geoFileFixture(geoRowUS)} // no GB row
	c := newGeoClient(t, g)
	got, err := c.resolveGeoTargets(context.Background(), []string{"US", "GB"})
	if err == nil {
		t.Fatal("expected an unresolved-code error, got nil")
	}
	if got != nil {
		t.Fatalf("no ids may be returned on a partial resolution, got %v", got)
	}
}

// A fetch failure must be an ERROR, never an empty map treated as "no targeting".
func TestResolveGeoTargets_FetchFailureIsAnError(t *testing.T) {
	g := &geoAPI{fileStatus: http.StatusInternalServerError, fileBody: geoFileFixture(geoRowUS)}
	c := newGeoClient(t, g)
	if _, err := c.resolveGeoTargets(context.Background(), []string{"US"}); err == nil {
		t.Fatal("a locations-file download failure must be an error, not silent no-targeting")
	}
}

// ---- caching ---------------------------------------------------------------

func TestGeoLocations_CachedAcrossCallsAndCoalesced(t *testing.T) {
	g := &geoAPI{fileBody: geoFileFixture(geoRowUS, geoRowGB)}
	c := newGeoClient(t, g)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.resolveGeoTargets(context.Background(), []string{"US"}); err != nil {
				t.Errorf("resolveGeoTargets: %v", err)
			}
		}()
	}
	wg.Wait()
	// A second round after the first has settled must be served from cache.
	if _, err := c.resolveGeoTargets(context.Background(), []string{"GB"}); err != nil {
		t.Fatalf("resolveGeoTargets: %v", err)
	}
	if n := g.downloads.Load(); n != 1 {
		t.Fatalf("locations file downloaded %d times, want 1 (cache + single-flight)", n)
	}
}

// ---- the CampaignCriterions wire contract ----------------------------------

func TestCreateCampaignCriterions_SendsTargetsCriterionTypeAndLocationIds(t *testing.T) {
	var seen createCampaignCriterionsRequest
	g := &geoAPI{
		fileBody:      geoFileFixture(geoRowUS, geoRowJP),
		criterionSeen: &seen,
		criterionBody: `{"CampaignCriterionIds":[9001,9002],"NestedPartialErrors":[]}`,
	}
	c := newGeoClient(t, g)
	ids, err := c.createCampaignCriterions(context.Background(), "321", []string{"190", "182"})
	if err != nil {
		t.Fatalf("createCampaignCriterions: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids = %v, want 2", ids)
	}
	// CriterionType MUST be "Targets" — Microsoft rejects "Location" on Add.
	if seen.CriterionType != "Targets" {
		t.Errorf("CriterionType = %q, want %q", seen.CriterionType, "Targets")
	}
	if len(seen.CampaignCriterions) != 2 {
		t.Fatalf("sent %d criterions, want 2", len(seen.CampaignCriterions))
	}
	first := seen.CampaignCriterions[0]
	if first.Type != "BiddableCampaignCriterion" {
		t.Errorf("CampaignCriterion.Type = %q, want BiddableCampaignCriterion", first.Type)
	}
	if first.Criterion.Type != "LocationCriterion" {
		t.Errorf("Criterion.Type = %q, want LocationCriterion", first.Criterion.Type)
	}
	if first.Criterion.LocationId.String() != "190" {
		t.Errorf("LocationId = %q, want 190", first.Criterion.LocationId.String())
	}
	if first.CampaignId.String() != "321" {
		t.Errorf("CampaignId = %q, want 321", first.CampaignId.String())
	}
	// The read-only elements must NOT be sent: DisplayName/LocationType are Add:Read-only.
	raw, _ := json.Marshal(first.Criterion)
	for _, banned := range []string{"DisplayName", "LocationType", "EnclosedLocationIds"} {
		if bytes.Contains(raw, []byte(banned)) {
			t.Errorf("criterion body must not carry the Add:Read-only element %q: %s", banned, raw)
		}
	}
}

// NestedPartialErrors is the shape AddCampaignCriterions returns — NOT the flat PartialErrors
// every other create in this client uses. A flat decode sees zero errors here and reports a
// REJECTED criterion as success, i.e. an untargeted campaign reported as targeted.
func TestCreateCampaignCriterions_DetectsNestedPartialErrors(t *testing.T) {
	g := &geoAPI{
		fileBody: geoFileFixture(geoRowUS),
		criterionBody: `{"CampaignCriterionIds":[null],"NestedPartialErrors":[` +
			`{"BatchErrors":[{"Code":1234,"ErrorCode":"CampaignServiceInvalidLocationCriterion","Index":0}],"Index":0}]}`,
	}
	c := newGeoClient(t, g)
	_, err := c.createCampaignCriterions(context.Background(), "321", []string{"190"})
	if err == nil {
		t.Fatal("a NestedPartialErrors rejection must be reported, not read as success")
	}
	if !errors.Is(err, errPartialFailure) {
		t.Fatalf("error = %v, want errPartialFailure", err)
	}
	if !strings.Contains(err.Error(), "CampaignServiceInvalidLocationCriterion") {
		t.Errorf("error %q should surface the nested error code", err)
	}
}

func TestCreateCampaignCriterions_ShortIDArrayIsUnconfirmed(t *testing.T) {
	g := &geoAPI{
		fileBody:      geoFileFixture(geoRowUS, geoRowJP),
		criterionBody: `{"CampaignCriterionIds":[9001],"NestedPartialErrors":[]}`,
	}
	c := newGeoClient(t, g)
	_, err := c.createCampaignCriterions(context.Background(), "321", []string{"190", "182"})
	if err == nil {
		t.Fatal("a short id array must not be reported as success")
	}
	if !errors.Is(err, errNoID) {
		t.Fatalf("error = %v, want errNoID (UNCONFIRMED)", err)
	}
}

// ---- end to end through CreateCampaign -------------------------------------

func TestCreateCampaign_AttachesGeoTargetsAtCampaignLevel(t *testing.T) {
	var seen createCampaignCriterionsRequest
	g := &geoAPI{
		fileBody:      geoFileFixture(geoRowUS, geoRowGB),
		criterionSeen: &seen,
		criterionBody: `{"CampaignCriterionIds":[9001,9002],"NestedPartialErrors":[]}`,
	}
	c := newGeoClient(t, g)
	in := validInput()
	in.GeoTargets = []string{"US", "GB"}

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if !g.sawPath("/CampaignCriterions") {
		t.Fatal("no /CampaignCriterions request was sent — GeoTargets never reached the API")
	}
	if len(res.GeoCriterionIDs) != 2 {
		t.Errorf("GeoCriterionIDs = %v, want 2 ids", res.GeoCriterionIDs)
	}
	if len(seen.CampaignCriterions) != 2 {
		t.Fatalf("sent %d location criteria, want 2", len(seen.CampaignCriterions))
	}
	// Campaign-level: each criterion carries the CAMPAIGN id created above.
	for _, cc := range seen.CampaignCriterions {
		if cc.CampaignId.String() != "321" {
			t.Errorf("criterion CampaignId = %q, want the created campaign 321", cc.CampaignId.String())
		}
	}
}

func TestCreateCampaign_NoGeoTargetsSendsNoCriterionRequest(t *testing.T) {
	g := &geoAPI{fileBody: geoFileFixture(geoRowUS)}
	c := newGeoClient(t, g)
	in := validInput() // GeoTargets unset

	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if g.sawPath("/CampaignCriterions") {
		t.Error("no geo targets were supplied, so no /CampaignCriterions request may be sent")
	}
	if g.sawPath("/GeoLocationsFileUrl/Query") {
		t.Error("no geo targets were supplied, so the locations file must not be fetched")
	}
}

// ---- FAIL CLOSED -----------------------------------------------------------
//
// The load-bearing tests. Each asserts that NO MUTATING CALL WAS ISSUED — not merely that an
// error was returned. A test that only checked the error would pass against an implementation
// that created the campaign first and validated afterwards, which is exactly the orphaned-paid
// -campaign defect this ordering exists to prevent.

func TestCreateCampaign_UnsupportedGeoCodeCreatesNothing(t *testing.T) {
	g := &geoAPI{fileBody: geoFileFixture(geoRowUS)}
	c := newGeoClient(t, g)
	in := validInput()
	in.GeoTargets = []string{"US", "USA"} // "USA" is not an ISO-2 code

	if _, err := c.CreateCampaign(context.Background(), in); err == nil {
		t.Fatal("an unsupported geo code must fail the create")
	}
	if got := g.mutatingPaths(); len(got) != 0 {
		t.Fatalf("NO mutating call may be issued when a geo code is invalid; got %v", got)
	}
}

func TestCreateCampaign_UnresolvableGeoCodeCreatesNothing(t *testing.T) {
	// "DE" is a supported ISO code but has no row in this file, so it cannot resolve.
	g := &geoAPI{fileBody: geoFileFixture(geoRowUS, geoRowGB)}
	c := newGeoClient(t, g)
	in := validInput()
	in.GeoTargets = []string{"US", "DE"}

	_, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("an unresolvable geo code must fail the create")
	}
	if !errors.Is(err, errGeoUnresolved) {
		t.Fatalf("error = %v, want errGeoUnresolved", err)
	}
	if got := g.mutatingPaths(); len(got) != 0 {
		t.Fatalf("NO mutating call may be issued when a geo code cannot be resolved; got %v", got)
	}
	// Specifically: the campaign itself must not exist.
	if g.sawPath("/Campaigns") && !g.sawPath("/Campaigns/QueryByAccountId") {
		t.Fatal("the campaign create must not have been issued")
	}
}

func TestCreateCampaign_LocationsFileUnavailableCreatesNothing(t *testing.T) {
	g := &geoAPI{fileStatus: http.StatusServiceUnavailable, fileBody: geoFileFixture(geoRowUS)}
	c := newGeoClient(t, g)
	in := validInput()
	in.GeoTargets = []string{"US"}

	if _, err := c.CreateCampaign(context.Background(), in); err == nil {
		t.Fatal("an unreachable locations file must fail the create rather than skip targeting")
	}
	if got := g.mutatingPaths(); len(got) != 0 {
		t.Fatalf("NO mutating call may be issued when the locations file is unavailable; got %v", got)
	}
}

// A geo attach that is REJECTED must surface as an error carrying the campaign id, never as a
// success — a campaign with no location criteria serves everywhere.
func TestCreateCampaign_RejectedGeoAttachIsNotReportedAsSuccess(t *testing.T) {
	g := &geoAPI{
		fileBody: geoFileFixture(geoRowUS),
		criterionBody: `{"CampaignCriterionIds":[null],"NestedPartialErrors":[` +
			`{"BatchErrors":[{"ErrorCode":"CampaignServiceInvalidLocationCriterion","Index":0}],"Index":0}]}`,
	}
	c := newGeoClient(t, g)
	in := validInput()
	in.GeoTargets = []string{"US"}

	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("a rejected geo attach must not be reported as a successful create")
	}
	if res == nil || res.CampaignID == "" {
		t.Fatal("the partial must carry the campaign id so the tree can be reconciled")
	}
	// The ad group/ad cascade must NOT have run: the campaign is untargeted, so building more
	// of the tree on top of it only makes the un-targeted campaign more enable-able.
	if g.sawPath("/AdGroups") {
		t.Error("the cascade must stop when geo targeting was rejected")
	}
}

// The geo attach must happen BEFORE the ad-group cascade, so the window in which a campaign
// exists without targeting is as small as possible.
func TestCreateCampaign_GeoAttachPrecedesAdGroupCreate(t *testing.T) {
	g := &geoAPI{
		fileBody:      geoFileFixture(geoRowUS),
		criterionBody: `{"CampaignCriterionIds":[9001],"NestedPartialErrors":[]}`,
	}
	c := newGeoClient(t, g)
	in := validInput()
	in.GeoTargets = []string{"US"}
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	geoAt, adGroupAt := -1, -1
	for i, p := range g.paths {
		if geoAt == -1 && strings.HasSuffix(p, "/CampaignCriterions") {
			geoAt = i
		}
		if adGroupAt == -1 && strings.HasSuffix(p, "/AdGroups") {
			adGroupAt = i
		}
	}
	if geoAt == -1 || adGroupAt == -1 {
		t.Fatalf("expected both a geo attach and an ad-group create, got %v", g.paths)
	}
	if geoAt > adGroupAt {
		t.Fatalf("geo attach (index %d) must precede the ad-group create (index %d): %v", geoAt, adGroupAt, g.paths)
	}
}

// The locations-file download must NOT carry the API credentials: the FileUrl is a pre-signed
// storage URL on a different host, and attaching the developer token / bearer would leak them.
func TestDownloadGeoFile_SendsNoCredentials(t *testing.T) {
	var gotAuth, gotDev string
	fileSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDev = r.Header.Get("DeveloperToken")
		_, _ = io.WriteString(w, geoFileFixture(geoRowUS))
	}))
	t.Cleanup(fileSrv.Close)

	g := &geoAPI{}
	c := newAPIClient(t, g.handler(t))
	if _, err := c.downloadGeoFile(context.Background(), fileSrv.URL+"/geolocations.csv"); err != nil {
		t.Fatalf("downloadGeoFile: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("the storage download must not carry an Authorization header, got %q", gotAuth)
	}
	if gotDev != "" {
		t.Errorf("the storage download must not carry a DeveloperToken header, got %q", gotDev)
	}
}
