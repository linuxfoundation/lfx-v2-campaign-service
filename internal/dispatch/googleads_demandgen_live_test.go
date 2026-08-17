// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

// This file is the G6 integration slice: the Demand Gen dispatch reads its three role image
// assets from the REAL reused creative-asset repository (Postgres), not a fake reader, and drives
// them through to a Demand Gen ad against a stubbed Google Ads server. The unit tests in
// googleads_test.go already cover the resolution logic against a fake reader; what only a real
// database proves is that the (project, brief, asset-id) scope the dispatcher passes to GetAsset
// resolves the bytes the upload actually stored, and that a reference to an asset that is not
// there fails the dispatch through the same claim-releasing pre-create path — the two properties
// that would silently regress if the repo's scoping SQL or the dispatcher's argument order drifted
// apart. It mirrors creative_asset_repo_live_test.go's live convention (skip off CI, fail on CI
// when TEST_DATABASE_URL is unset — a skipped live test on a runner that promised a database is a
// green build for a suite that never ran) rather than the fake-reader unit convention.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

var (
	demandGenLivePoolOnce sync.Once
	demandGenLivePool     *postgres.Pool
	demandGenLivePoolErr  error
)

// demandGenLiveTestPool stands up a migrated *postgres.Pool against TEST_DATABASE_URL. It repeats
// the minimal bootstrap creative_asset_repo_live_test.go uses rather than sharing it: that helper
// is unexported in package postgres, and this test is in package dispatch (it needs the in-package
// connection fakes activeGoogleAdsConn/fakeConnReader/identityEncryptor), so the two cannot be one
// function. postgres.NewPool and postgres.Migrate are the exported entry points, and package
// dispatch importing package postgres is cycle-free — postgres imports neither dispatch nor
// anything that reaches it.
func demandGenLiveTestPool(t *testing.T) *postgres.Pool {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL is empty on a CI runner: this live-database test would skip entirely")
		}
		t.Skip("TEST_DATABASE_URL is not set; skipping the live-database test")
	}

	demandGenLivePoolOnce.Do(func() {
		if err := postgres.Migrate(dsn); err != nil {
			demandGenLivePoolErr = err
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		demandGenLivePool, demandGenLivePoolErr = postgres.NewPool(ctx, dsn)
	})
	if demandGenLivePoolErr != nil {
		t.Fatalf("live-database harness: %v", demandGenLivePoolErr)
	}
	return demandGenLivePool
}

// insertActiveBriefForDemandGen inserts an ACTIVE parent brief and returns its id and project, so
// the creative-asset write's INSERT ... WHERE EXISTS active-parent gate accepts the three role
// uploads. Mirrors insertCreativeAssetTestBrief in the postgres live test (a distinct project per
// call keeps rows from colliding across runs of a never-dropped shared schema).
func insertActiveBriefForDemandGen(ctx context.Context, t *testing.T, pool *postgres.Pool) (briefID, projectID string) {
	t.Helper()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("cannot generate a unique project id: %v", err)
	}
	projectID = "demandgen-live-" + hex.EncodeToString(suffix[:])
	err := pool.QueryRow(ctx, `
		INSERT INTO campaign_briefs (project_id, program_type, event_slug, status)
		VALUES ($1, 'events', $1, 'approved')
		RETURNING id`, projectID).Scan(&briefID)
	if err != nil {
		t.Fatalf("insert active parent brief: %v", err)
	}
	return briefID, projectID
}

// storeRoleAsset writes one role image under the (project, brief) via the REAL repo and returns its
// stored id. The checksum is the true SHA-256 of the bytes (the repo's dedupe key), distinct per
// role because the bytes differ, so no two roles collide on (brief_id, checksum).
func storeRoleAsset(ctx context.Context, t *testing.T, repo *postgres.CreativeAssetRepo, projectID, briefID string, imgBytes []byte) string {
	t.Helper()
	sum := sha256.Sum256(imgBytes)
	stored, err := repo.CreateAsset(ctx, &model.CreativeAsset{
		ProjectID: projectID,
		BriefID:   briefID,
		MimeType:  model.MimeTypePNG,
		ByteSize:  int64(len(imgBytes)),
		Checksum:  hex.EncodeToString(sum[:]),
		Bytes:     imgBytes,
		CreatedBy: json.RawMessage(`{"principal":"demandgen-live-test"}`),
	})
	if err != nil {
		t.Fatalf("store role asset: %v", err)
	}
	return stored.ID
}

// liveBrief builds the dispatch brief whose (ID, ProjectID) match the DB rows. The dispatcher never
// re-reads the brief from Postgres — it only passes brief.ProjectID/brief.ID to GetAsset — so the
// EventDetails here mirror testBrief() purely so ComposeName/composeAdCopy have the same inputs the
// unit wire test gives them.
func liveBrief(briefID, projectID string) *model.CampaignBrief {
	return &model.CampaignBrief{
		ID:           briefID,
		ProjectID:    projectID,
		EventSlug:    "kubecon-na-2026",
		EventDetails: json.RawMessage(`{"eventName":"KubeCon NA 2026","registrationUrl":"https://events.example/kc","project":"cncf"}`),
	}
}

// TestGoogleAds_Dispatch_DemandGen_LoadsBytesFromRealRepo is the G6 integration happy path: three
// role images are uploaded through the real creative-asset repo, then a demand-gen dispatch wired
// to that same repo resolves them by id and uploads their exact stored bytes to the stubbed Google
// Ads server before the budget mutate, and the created ad id lands on the persisted campaign. A
// regression that broke the repo's (project, brief) scoping or the dispatcher's GetAsset argument
// order would surface here as resolved-nothing or wrong-bytes, which no fake-reader unit test can.
func TestGoogleAds_Dispatch_DemandGen_LoadsBytesFromRealRepo(t *testing.T) {
	pool := demandGenLiveTestPool(t)
	ctx := context.Background()
	repo := postgres.NewCreativeAssetRepo(pool)

	briefID, projectID := insertActiveBriefForDemandGen(ctx, t, pool)
	// Distinct, recognizable bytes per role so the wire assertion below can prove each role's
	// stored image — not some other role's — reached the upload.
	marketingBytes := []byte("\x89PNG\r\n\x1a\nMARKETING-" + briefID)
	squareBytes := []byte("\x89PNG\r\n\x1a\nSQUARE-" + briefID)
	logoBytes := []byte("\x89PNG\r\n\x1a\nLOGO-" + briefID)
	marketingID := storeRoleAsset(ctx, t, repo, projectID, briefID, marketingBytes)
	squareID := storeRoleAsset(ctx, t, repo, projectID, briefID, squareBytes)
	logoID := storeRoleAsset(ctx, t, repo, projectID, briefID, logoBytes)

	var (
		mu             sync.Mutex
		assetBodies    [][]byte
		budgetSeen     bool
		assetsBeforeBg bool
		adSeen         bool
		assetCounter   int
	)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "assets:mutate"):
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			assetBodies = append(assetBodies, body)
			assetCounter++
			id := 400 + assetCounter
			mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/assets/`+strconv.Itoa(id)+`"}]}`)
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			mu.Lock()
			budgetSeen = true
			assetsBeforeBg = len(assetBodies) == 3 // all three assets uploaded before any spend
			mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/333"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			mu.Lock()
			adSeen = true
			mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupAds/333~444"}]}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	d.SetCreativeAssetRepo(repo)

	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"demand-gen","creative":{
		"mediaFormat":"single_image","businessName":"CNCF",
		"marketingImageAssetId":"` + marketingID + `",
		"squareMarketingImageAssetId":"` + squareID + `",
		"logoAssetId":"` + logoID + `"}}}`)
	camp, err := d.Dispatch(ctx, liveBrief(briefID, projectID), model.ProviderGoogleAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(assetBodies) != 3 {
		t.Fatalf("expected 3 asset uploads, got %d", len(assetBodies))
	}
	if !budgetSeen || !adSeen {
		t.Errorf("expected both a budget mutate (%v) and an ad mutate (%v)", budgetSeen, adSeen)
	}
	if !assetsBeforeBg {
		t.Error("all three image assets must upload BEFORE the budget (fail before spending)")
	}
	// The EXACT bytes each role was stored with must reach the wire as base64 — proof the repo
	// round-tripped the image and the dispatcher resolved the right id to the right role.
	joined := string(bytes.Join(assetBodies, []byte("|")))
	for role, want := range map[string][]byte{
		"marketing": marketingBytes, "square": squareBytes, "logo": logoBytes,
	} {
		b64 := base64.StdEncoding.EncodeToString(want)
		if !strings.Contains(joined, b64) {
			t.Errorf("upload bodies missing the %s image bytes (base64 %q) — the repo bytes did not reach the wire", role, b64)
		}
	}
	if camp == nil || len(camp.Result) == 0 || !strings.Contains(string(camp.Result), "444") {
		t.Errorf("persisted result must carry the created ad id 444, got %s", string(camp.Result))
	}
	if !strings.Contains(string(camp.Result), `"channel":"demand-gen"`) {
		t.Errorf("persisted result must record the resolved channel, got %s", string(camp.Result))
	}
}

// TestGoogleAds_Dispatch_DemandGen_MissingAssetFailsCleanly is the G6 integration failure path: a
// demand-gen dispatch that references a role asset id NOT present under the brief must fail before
// any upstream create, through the claim-releasing NoUpstreamCreate path — never spending a budget
// or stranding a claim on a dangling reference. Two roles exist in the DB; the logo id is a
// well-formed UUID that was never stored, so the repo returns ErrNotFound and resolution stops.
func TestGoogleAds_Dispatch_DemandGen_MissingAssetFailsCleanly(t *testing.T) {
	pool := demandGenLiveTestPool(t)
	ctx := context.Background()
	repo := postgres.NewCreativeAssetRepo(pool)

	briefID, projectID := insertActiveBriefForDemandGen(ctx, t, pool)
	marketingID := storeRoleAsset(ctx, t, repo, projectID, briefID, []byte("\x89PNG\r\n\x1a\nMARKETING-"+briefID))
	squareID := storeRoleAsset(ctx, t, repo, projectID, briefID, []byte("\x89PNG\r\n\x1a\nSQUARE-"+briefID))

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An asset upload may legitimately precede the failing resolution only if resolution ran
		// per-role and uploaded before checking the missing one — it does NOT: resolveDemandGenCreative
		// resolves all three ids to bytes up front, so a missing role stops the dispatch before the
		// client is ever called. Any request here is a regression.
		t.Errorf("no Google Ads request may be issued when a role asset is missing, got %s", r.URL.Path)
		http.Error(w, "should not be reached", http.StatusInternalServerError)
	}))
	t.Cleanup(apiSrv.Close)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	d.SetCreativeAssetRepo(repo)

	// A well-formed UUID that was never stored under this brief → GetAsset ErrNotFound.
	const missingLogoID = "00000000-0000-4000-8000-000000000000"
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"demand-gen","creative":{
		"mediaFormat":"single_image","businessName":"CNCF",
		"marketingImageAssetId":"` + marketingID + `",
		"squareMarketingImageAssetId":"` + squareID + `",
		"logoAssetId":"` + missingLogoID + `"}}}`)
	camp, err := d.Dispatch(ctx, liveBrief(briefID, projectID), model.ProviderGoogleAds, cfg)
	if camp != nil {
		t.Errorf("a missing role asset must return a nil campaign, got %+v", camp)
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a missing role asset must be NoUpstreamCreate (release the claim), got %T: %v", err, err)
	}
}
