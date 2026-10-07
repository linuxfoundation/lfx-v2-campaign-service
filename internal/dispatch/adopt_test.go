// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// The capability is reached by TYPE ASSERTION (Orchestrator.LookupPlatformCampaign), so a
// drifted signature would silently turn adoption back into a 400 "not supported". These make
// that a compile error.
var (
	_ service.CampaignAdopter = (*MicrosoftDispatcher)(nil)
	_ service.CampaignAdopter = (*MetaDispatcher)(nil)
	_ service.CampaignAdopter = (*RedditDispatcher)(nil)
	_ service.CampaignAdopter = (*TwitterDispatcher)(nil)
)

// adopterUnderTest is what each platform's dispatcher must be for these tests: an adopter, and
// the toggler whose ACTIVATE refusal is the other half of the adoption contract.
type adopterUnderTest interface {
	service.CampaignAdopter
	service.StatusToggler
}

// adoptStub is one fake platform: a token endpoint, the lookup, and any mutation. Every request is
// recorded under a mutex — the handler runs on the server's goroutine and the test reads the log
// from its own — and the handler never calls t.Fatal.
type adoptStub struct {
	mu           sync.Mutex
	seen         []string
	lookupStatus int
	lookupBody   string
	mutationBody string
	isLookup     func(*http.Request) bool
}

func (s *adoptStub) set(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookupStatus, s.lookupBody = status, body
}

func (s *adoptStub) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func (s *adoptStub) apiRequests() []string {
	var out []string
	for _, r := range s.requests() {
		if !strings.HasSuffix(r, " /token") {
			out = append(out, r)
		}
	}
	return out
}

func (s *adoptStub) serve(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	s.mu.Lock()
	s.seen = append(s.seen, r.Method+" "+r.URL.Path)
	status, body, mutation := s.lookupStatus, s.lookupBody, s.mutationBody
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/token":
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	case s.isLookup(r):
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	default:
		_, _ = io.WriteString(w, mutation)
	}
}

// adopterCase is one platform's fixture: its own connection row, how to build its dispatcher
// against the stub, the bodies its read answers with, and how its row reads provenance back.
type adopterCase struct {
	provider  model.Provider
	conn      *model.Connection
	build     func(repo connReader, base string) adopterUnderTest
	isLookup  func(*http.Request) bool
	validID   string
	malformed []string
	found     string
	foreign   string // a body reporting ANOTHER account; "" where the read is account-scoped
	absent    [2]any // status, body
	mutation  string
	// creationAccount is the platform's own reader of the row's provenance — the function every
	// later read, toggle and lever relies on.
	creationAccount func(*model.Campaign) string
	wantAccount     string
	wantName        string
}

func adopterCases() []adopterCase {
	msConn := activeMicrosoftConn(goodMicrosoftCreds)
	metaConn := activeMetaConn(goodMetaCreds)
	redditConn := activeRedditConn(goodRedditCreds)
	xConn := activeTwitterConn(goodTwitterCreds)
	for _, c := range []*model.Connection{msConn, metaConn, redditConn, xConn} {
		c.ID, c.Version = "conn-"+string(c.Provider), 1
	}
	return []adopterCase{
		{
			provider: model.ProviderMicrosoftAds, conn: msConn,
			build: func(repo connReader, base string) adopterUnderTest {
				return NewMicrosoftDispatcher(repo, identityEncryptor{}, microsoft.WithTokenURL(base+"/token"), microsoft.WithBaseURL(base))
			},
			isLookup:        func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/Campaigns/QueryByIds") },
			validID:         "321",
			malformed:       []string{"007", "0", "abc", " 321", "9223372036854775808"},
			found:           `{"Campaigns":[{"Id":321,"Name":"KubeCon — Search","Status":"Paused"}],"PartialErrors":[]}`,
			absent:          [2]any{http.StatusOK, `{"Campaigns":[null],"PartialErrors":[{"Code":1100,"ErrorCode":"CampaignServiceInvalidCampaignId","Index":0}]}`},
			mutation:        `{"PartialErrors":[]}`,
			creationAccount: microsoftCreationAccountID, wantAccount: "1234567", wantName: "KubeCon — Search",
		},
		{
			provider: model.ProviderMetaAds, conn: metaConn,
			build: func(repo connReader, base string) adopterUnderTest {
				return NewMetaDispatcher(repo, identityEncryptor{}, meta.WithBaseURL(base))
			},
			isLookup:        func(r *http.Request) bool { return r.Method == http.MethodGet && r.URL.Path == "/120200000000001" },
			validID:         "120200000000001",
			malformed:       []string{"0", "0123", "abc", "act_777", " 120200000000001"},
			found:           `{"id":"120200000000001","name":"KubeCon — Leads","status":"PAUSED","effective_status":"PAUSED","account_id":"777","objective":"OUTCOME_LEADS"}`,
			foreign:         `{"id":"120200000000001","name":"KubeCon — Leads","status":"ACTIVE","account_id":"888"}`,
			absent:          [2]any{http.StatusBadRequest, `{"error":{"message":"does not exist","type":"GraphMethodException","code":100,"error_subcode":33}}`},
			mutation:        `{"success":true}`,
			creationAccount: metaCreationAccountID, wantAccount: "act_777", wantName: "KubeCon — Leads",
		},
		{
			provider: model.ProviderRedditAds, conn: redditConn,
			build: func(repo connReader, base string) adopterUnderTest {
				return NewRedditDispatcher(repo, identityEncryptor{}, reddit.WithBaseURL(base+"/api/v3"), reddit.WithTokenURL(base+"/token"))
			},
			isLookup: func(r *http.Request) bool {
				return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/campaigns/t3_camp")
			},
			validID:         "t3_camp",
			malformed:       []string{"t3/../x", "t3?x=1", "t3-camp", " t3_camp"},
			found:           `{"data":{"id":"t3_camp","name":"KubeCon — Traffic","configured_status":"ACTIVE","ad_account_id":"t2_acct"}}`,
			foreign:         `{"data":{"id":"t3_camp","name":"KubeCon — Traffic","configured_status":"ACTIVE","ad_account_id":"t2_other"}}`,
			absent:          [2]any{http.StatusNotFound, `{}`},
			mutation:        `{"data":{"id":"t3_camp"}}`,
			creationAccount: redditCreationAccountID, wantAccount: "t2_acct", wantName: "KubeCon — Traffic",
		},
		{
			provider: model.ProviderTwitterAds, conn: xConn,
			build: func(repo connReader, base string) adopterUnderTest {
				return NewTwitterDispatcher(repo, identityEncryptor{}, twitter.WithBaseURL(base), twitter.WithWriteDelay(0))
			},
			isLookup: func(r *http.Request) bool {
				return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1/campaigns/cmp1")
			},
			validID:         "cmp1",
			malformed:       []string{"cmp/1", "cmp_1", " cmp1", "cmp?1"},
			found:           `{"data":{"id":"cmp1","name":"KubeCon — Awareness","entity_status":"PAUSED","deleted":false}}`,
			foreign:         `{"data":{"id":"cmp1","name":"KubeCon — Awareness","entity_status":"PAUSED","account_id":"acc9"}}`,
			absent:          [2]any{http.StatusNotFound, `{"errors":[{"code":"NOT_FOUND"}]}`},
			mutation:        `{"data":{"id":"cmp1"}}`,
			creationAccount: twitterCreationAccountID, wantAccount: "acc1", wantName: "KubeCon — Awareness",
		},
	}
}

// newAdopter wires a platform's dispatcher against a fresh stub, with the PROJECT'S OWN row and
// an LF system row in the repository — so a resolver that fell back would find something.
func newAdopter(t *testing.T, tc adopterCase) (adopterUnderTest, *adoptStub, *scopedConnReader) {
	t.Helper()
	stub := &adoptStub{lookupStatus: http.StatusOK, lookupBody: tc.found, mutationBody: tc.mutation, isLookup: tc.isLookup}
	srv := httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(srv.Close)
	repo := &scopedConnReader{rows: map[string]*model.Connection{"cncf": tc.conn, model.SystemProjectID: tc.conn}}
	return tc.build(repo, srv.URL), stub, repo
}

// The happy path, and the contract that makes it safe: one read, the platform's own id and name,
// the single slot, and a result that records the verified account and NOTHING else — so the
// platform's own provenance reader sees the account, and its toggle refuses ACTIVATE.
func TestAdopters_FoundCampaignIsBoundWithProvenanceOnly(t *testing.T) {
	for _, tc := range adopterCases() {
		t.Run(string(tc.provider), func(t *testing.T) {
			d, stub, _ := newAdopter(t, tc)
			ref, err := d.LookupCampaign(context.Background(), "cncf", tc.provider, tc.validID)
			if err != nil {
				t.Fatalf("LookupCampaign: %v", err)
			}
			if ref == nil || ref.ID != tc.validID || ref.Name != tc.wantName || ref.Variant != model.VariantDefault {
				t.Fatalf("ref = %+v", ref)
			}
			if got := stub.apiRequests(); len(got) != 1 {
				t.Errorf("want exactly ONE platform read, got %q", got)
			}
			row := &model.Campaign{ID: "row-1", Platform: tc.provider, PlatformCampaignID: ref.ID, Result: ref.Result, Status: model.CampaignStatusCreated}
			if got := tc.creationAccount(row); got != tc.wantAccount {
				t.Errorf("the row's recorded account = %q, want %q — without it every account-mismatch guard waves the row through", got, tc.wantAccount)
			}
			for _, child := range []string{"adGroupId", "AdSetID", "LineItemID", "adId", "keywordIds"} {
				if strings.Contains(string(ref.Result), `"`+child+`":"`) && !strings.Contains(string(ref.Result), `"`+child+`":""`) {
					t.Errorf("the adopted result records a child id (%s): %s", child, ref.Result)
				}
			}

			// ACTIVATE is refused locally — the row proves no servable tree — and never
			// reaches the platform.
			before := len(stub.apiRequests())
			terr := d.ToggleStatus(context.Background(), "cncf", tc.provider, row, model.CampaignRunActive)
			if !errors.Is(terr, domain.ErrCampaignNotProvisioned) {
				t.Fatalf("ACTIVATE on an adopted row = %v, want ErrCampaignNotProvisioned", terr)
			}
			if !strings.Contains(terr.Error(), "ADOPTED") {
				t.Errorf("the refusal %q does not say adoption is a reason, so an operator is sent to re-provision a campaign that was bound on purpose", terr)
			}
			if got := stub.apiRequests(); len(got) != before {
				t.Errorf("a refused ACTIVATE reached the platform: %q", got[before:])
			}

			// PAUSE is allowed, and addresses the campaign alone: there are no children to cascade to.
			if perr := d.ToggleStatus(context.Background(), "cncf", tc.provider, row, model.CampaignRunPaused); perr != nil {
				t.Fatalf("PAUSE on an adopted row: %v", perr)
			}
			mutations := stub.apiRequests()[before:]
			if len(mutations) != 1 {
				t.Errorf("PAUSE sent %q, want exactly one campaign-level mutation", mutations)
			}
		})
	}
}

// A malformed id is a permanent input fault: 400 whatever the connection state, decided before
// any connection is loaded or any request is sent.
func TestAdopters_MalformedIDIsRefusedBeforeAnyConnectionWork(t *testing.T) {
	for _, tc := range adopterCases() {
		t.Run(string(tc.provider), func(t *testing.T) {
			d, stub, repo := newAdopter(t, tc)
			for _, id := range append([]string{""}, tc.malformed...) {
				ref, err := d.LookupCampaign(context.Background(), "cncf", tc.provider, id)
				if !errors.Is(err, domain.ErrInvalidPlatformCampaignID) || ref != nil {
					t.Errorf("LookupCampaign(%q) = %+v, %v; want ErrInvalidPlatformCampaignID", id, ref, err)
				}
			}
			if len(repo.gets) != 0 {
				t.Errorf("a malformed id loaded connections %q; it must be refused first", repo.gets)
			}
			if got := stub.requests(); len(got) != 0 {
				t.Errorf("a malformed id reached the platform: %q", got)
			}
		})
	}
}

// Adoption never borrows the LF system account: a project with no connection of its own gets the
// actionable 409 sentinel, and the system scope is never even read.
func TestAdopters_RefuseTheSystemFallback(t *testing.T) {
	for _, tc := range adopterCases() {
		t.Run(string(tc.provider), func(t *testing.T) {
			d, stub, repo := newAdopter(t, tc)
			delete(repo.rows, "cncf")
			_, err := d.LookupCampaign(context.Background(), "cncf", tc.provider, tc.validID)
			if !errors.Is(err, domain.ErrAdoptionRequiresOwnConnection) {
				t.Fatalf("err = %v, want ErrAdoptionRequiresOwnConnection", err)
			}
			for _, got := range repo.gets {
				if got == model.SystemProjectID {
					t.Fatalf("the LF system scope was read on the adoption path")
				}
			}
			if got := stub.requests(); len(got) != 0 {
				t.Errorf("the platform was contacted without a connection of the project's own: %q", got)
			}
		})
	}
}

// A connection that cannot be used is the 409 "repair the connection" answer, decided before any
// request — never a 503 that promises a retry could help.
func TestAdopters_UnusableOwnConnectionIsNotUnverifiable(t *testing.T) {
	for _, tc := range adopterCases() {
		t.Run(string(tc.provider), func(t *testing.T) {
			d, stub, repo := newAdopter(t, tc)
			broken := *tc.conn
			broken.AccountID = ""
			repo.rows["cncf"] = &broken
			_, err := d.LookupCampaign(context.Background(), "cncf", tc.provider, tc.validID)
			if !errors.Is(err, domain.ErrConnectionNotUsable) || !errors.Is(err, domain.ErrAccountNotSelected) {
				t.Fatalf("err = %v, want ErrConnectionNotUsable + ErrAccountNotSelected", err)
			}
			if got := stub.apiRequests(); len(got) != 0 {
				t.Errorf("an account-less connection reached the platform: %q", got)
			}
		})
	}
}

// "The platform answered: no such campaign" is (nil, nil) — and the orchestrator turns that into
// the 404 sentinel, never into success.
func TestAdopters_AbsentCampaignIsAnAnsweredAbsence(t *testing.T) {
	for _, tc := range adopterCases() {
		t.Run(string(tc.provider), func(t *testing.T) {
			d, stub, _ := newAdopter(t, tc)
			stub.set(tc.absent[0].(int), tc.absent[1].(string))
			ref, err := d.LookupCampaign(context.Background(), "cncf", tc.provider, tc.validID)
			if err != nil || ref != nil {
				t.Fatalf("want (nil, nil), got %+v, %v", ref, err)
			}
			orch := service.NewOrchestrator(nil, nil, map[model.Provider]service.PlatformDispatcher{tc.provider: d.(service.PlatformDispatcher)})
			_, oerr := orch.LookupPlatformCampaign(context.Background(), "cncf", tc.provider, tc.validID)
			if !errors.Is(oerr, service.ErrPlatformCampaignAbsent) {
				t.Errorf("orchestrator err = %v, want ErrPlatformCampaignAbsent", oerr)
			}
		})
	}
}

// A campaign the platform reports under ANOTHER account is refused as a mismatch: it exists, so
// it must not read as absent, and it is not this project's to bind.
func TestAdopters_ForeignAccountIsAMismatchNotAnAbsence(t *testing.T) {
	for _, tc := range adopterCases() {
		if tc.foreign == "" {
			continue // account-scoped read: a foreign campaign is answered as absent by the platform
		}
		t.Run(string(tc.provider), func(t *testing.T) {
			d, stub, _ := newAdopter(t, tc)
			stub.set(http.StatusOK, tc.foreign)
			ref, err := d.LookupCampaign(context.Background(), "cncf", tc.provider, tc.validID)
			if !errors.Is(err, domain.ErrCampaignAccountMismatch) || ref != nil {
				t.Fatalf("got %+v, %v; want ErrCampaignAccountMismatch", ref, err)
			}
		})
	}
}

// Everything the read could not verify is an ERROR carrying no sentinel — the service's 503
// "could not be verified" — never an absence and never a binding.
func TestAdopters_UnverifiableAnswersAreErrors(t *testing.T) {
	for _, tc := range adopterCases() {
		t.Run(string(tc.provider), func(t *testing.T) {
			for _, ans := range []struct {
				name   string
				status int
				body   string
			}{
				{"5xx", http.StatusInternalServerError, `{}`},
				{"malformed body", http.StatusOK, `{"`},
				{"unknown status", http.StatusOK, strings.NewReplacer(`"Paused"`, `"Mystery"`, `"PAUSED"`, `"MYSTERY"`, `"ACTIVE"`, `"MYSTERY"`).Replace(tc.found)},
			} {
				t.Run(ans.name, func(t *testing.T) {
					d, stub, _ := newAdopter(t, tc)
					stub.set(ans.status, ans.body)
					ref, err := d.LookupCampaign(context.Background(), "cncf", tc.provider, tc.validID)
					if err == nil || ref != nil {
						t.Fatalf("want an error, got %+v, %v", ref, err)
					}
					for _, sentinel := range []error{domain.ErrInvalidPlatformCampaignID, domain.ErrAdoptionRequiresOwnConnection,
						domain.ErrConnectionNotUsable, domain.ErrCampaignAccountMismatch, domain.ErrPlatformCampaignAbsent} {
						if errors.Is(err, sentinel) {
							t.Errorf("an unverifiable answer carries %v, which the service answers as something other than 503", sentinel)
						}
					}
				})
			}
		})
	}
}

// The gate is a type assertion, so this is what proves the four platforms left the
// ErrAdoptionUnsupported path: through the real orchestrator, a malformed id now reaches each
// adapter's own validation instead of being refused as unsupported.
func TestAdopters_AreReachableThroughTheOrchestrator(t *testing.T) {
	for _, tc := range adopterCases() {
		t.Run(string(tc.provider), func(t *testing.T) {
			d, _, _ := newAdopter(t, tc)
			orch := service.NewOrchestrator(nil, nil, map[model.Provider]service.PlatformDispatcher{tc.provider: d.(service.PlatformDispatcher)})
			_, err := orch.LookupPlatformCampaign(context.Background(), "cncf", tc.provider, tc.malformed[0])
			if errors.Is(err, service.ErrAdoptionUnsupported) {
				t.Fatalf("%s is still unsupported: %v", tc.provider, err)
			}
			if !errors.Is(err, service.ErrInvalidPlatformCampaignID) {
				t.Errorf("err = %v, want ErrInvalidPlatformCampaignID from the adapter", err)
			}
		})
	}
}
