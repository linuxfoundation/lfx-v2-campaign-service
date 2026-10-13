// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// ---------------------------------------------------------------------------
// One campaign, read by id (LFXV2-2665): the settings readback, adoption and the bid lever's
// pre-write read all go through GetCampaignSettings.
//
// SOURCE. LinkedIn Marketing API, "Create and Manage Campaigns" (li-lms-2026-09),
// https://learn.microsoft.com/en-us/linkedin/marketing/integrations/ads/account-structure/create-and-manage-campaigns
// consulted 2026-10-13: the campaign is addressed as GET adAccounts/{adAccountId}/adCampaigns/{id};
// `account` is a sponsoredAccount URN, `campaignGroup` a sponsoredCampaignGroup URN, budgets
// (`dailyBudget`, `totalBudget`) and `unitCost` are {amount: "real number string",
// currencyCode}, `runSchedule.start`/`end` are epoch milliseconds (start inclusive, end
// exclusive), and `optimizationTargetType` (default NONE) selects manual, automated, target-cost
// or cost-cap bidding.
//
// THE READ IS ACCOUNT-SCOPED BY ITS PATH, and its body is trusted as identity evidence only when
// identityjson accepts the raw bytes and the echoed id is the one asked for — the same rules the
// Reddit, Meta and X adoption reads apply.
// ---------------------------------------------------------------------------

// ErrInvalidCampaignID marks a campaign id this client refuses before any request.
var ErrInvalidCampaignID = errors.New("linkedin: campaign id must be 1-19 digits without a leading zero")

// campaignIDRE is a LinkedIn campaign id: a `long`, so digits with no leading zero, at most 19
// of them. It is EXACTLY the Pattern resolve-linkedin-ads-campaign declares (pinned by
// internal/apivalidation), which cannot express int64's upper bound, so neither does this — the
// same bound the Microsoft route states.
var campaignIDRE = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

// ValidateCampaignID reports whether id is a usable LinkedIn campaign id. Nothing is trimmed —
// the id is compared verbatim against stored platform ids. It contacts nothing.
func ValidateCampaignID(id string) error {
	if !campaignIDRE.MatchString(id) {
		return fmt.Errorf("validate campaign id: %w", ErrInvalidCampaignID)
	}
	return nil
}

// ValidAccountID reports whether id can address an ad account path (digits only). It is the
// rule resolveAccountID applies, exposed so a dispatcher can classify a connection's stored id
// BEFORE anything is sent.
func ValidAccountID(id string) bool { return accountIDRE.MatchString(id) }

// LinkedIn campaign statuses this package reasons about beyond ACTIVE/PAUSED (schema `status`).
const (
	StatusDraft           = "DRAFT"
	StatusArchived        = "ARCHIVED"
	StatusCompleted       = "COMPLETED"
	StatusCanceled        = "CANCELED"
	StatusPendingDeletion = "PENDING_DELETION"
	StatusRemoved         = "REMOVED"
)

// CampaignSettings is one campaign's live configuration. Every optional value is a pointer, nil
// when LinkedIn did not report it — never a zero standing in for "not reported".
type CampaignSettings struct {
	// CampaignID is the id LinkedIn ECHOED, already checked against the requested one.
	CampaignID string
	// AccountID is the trailing id of the `account` URN; "" when not reported.
	AccountID string
	// CampaignGroupID is the trailing id of the `campaignGroup` URN; "" when not reported.
	CampaignGroupID string

	Name                   *string
	Status                 *string
	ObjectiveType          *string
	OptimizationTargetType *string
	CostType               *string

	// UnitCostMicros is unitCost.amount in micro-units of its currency.
	UnitCostMicros   *int64
	UnitCostCurrency string
	// UnitCostUnparseable: unitCost.amount was reported but is not a decimal this client reads
	// exactly (more than six decimal places, a negative, an exponent...).
	UnitCostUnparseable bool

	DailyBudgetMicros *int64
	TotalBudgetMicros *int64
	// BudgetCurrency is the currency of whichever budget was reported (daily first).
	BudgetCurrency string
	// BudgetUnparseable: a budget amount was reported but could not be read exactly.
	BudgetUnparseable bool

	// RunStart / RunEnd are runSchedule.start / end as UTC instants.
	RunStart *time.Time
	RunEnd   *time.Time
}

// Terminal reports whether the campaign is in a status from which it can never serve again:
// CANCELED ("permanently canceled"), PENDING_DELETION and REMOVED ("deleted, but must remain
// fetchable"). ARCHIVED and COMPLETED are NOT terminal — LinkedIn documents un-archiving, and a
// completed campaign's limit can be raised.
func (s *CampaignSettings) Terminal() bool {
	if s == nil || s.Status == nil {
		return false
	}
	switch *s.Status {
	case StatusCanceled, StatusPendingDeletion, StatusRemoved:
		return true
	}
	return false
}

// knownStatus reports whether status is one of the eight values the schema documents.
func knownStatus(status string) bool {
	switch status {
	case StatusActive, StatusPaused, StatusDraft, StatusArchived, StatusCompleted,
		StatusCanceled, StatusPendingDeletion, StatusRemoved:
		return true
	}
	return false
}

// moneyWire is a LinkedIn amount. Amount is kept RAW because the schema says "real number
// string" while a JSON number spelling of the same digits is accepted rather than failing the
// whole read on a serialisation change.
type moneyWire struct {
	Amount       json.RawMessage `json:"amount"`
	CurrencyCode *string         `json:"currencyCode"`
}

type campaignSettingsWire struct {
	ID                     flexibleID `json:"id"`
	Account                *string    `json:"account"`
	CampaignGroup          *string    `json:"campaignGroup"`
	Name                   *string    `json:"name"`
	Status                 *string    `json:"status"`
	ObjectiveType          *string    `json:"objectiveType"`
	OptimizationTargetType *string    `json:"optimizationTargetType"`
	CostType               *string    `json:"costType"`
	UnitCost               *moneyWire `json:"unitCost"`
	DailyBudget            *moneyWire `json:"dailyBudget"`
	TotalBudget            *moneyWire `json:"totalBudget"`
	RunSchedule            *struct {
		Start *int64 `json:"start"`
		End   *int64 `json:"end"`
	} `json:"runSchedule"`
}

// decimalRE is the exact decimal shape an amount must have to be read: no sign, no exponent.
var decimalRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,12})(\.[0-9]+)?$`)

// parseDecimalMicros converts a LinkedIn amount to integer micro-units WITHOUT a float, so an
// amount is either read exactly or not at all. nil,false = not reported; nil,true = reported
// but unreadable (wrong JSON kind, a shape decimalRE refuses, or significant digits beyond the
// sixth decimal place, which micro-units cannot hold).
func parseDecimalMicros(raw json.RawMessage) (*int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	s := string(raw)
	if raw[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return nil, true
		}
		s = str
	}
	if !decimalRE.MatchString(s) {
		return nil, true
	}
	whole, frac, _ := strings.Cut(s, ".")
	frac = strings.TrimRight(frac, "0")
	if len(frac) > 6 {
		return nil, true
	}
	frac += strings.Repeat("0", 6-len(frac))
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return nil, true
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return nil, true
	}
	v := w*1_000_000 + f
	return &v, false
}

// urnTrailingID returns the id after the last ':' of a URN of the given type, or "" (with
// ok=false) when the value is not such a URN.
func urnTrailingID(urn, kind string) (string, bool) {
	prefix := "urn:li:" + kind + ":"
	if !strings.HasPrefix(urn, prefix) {
		return "", false
	}
	id := strings.TrimPrefix(urn, prefix)
	if !accountIDRE.MatchString(id) {
		return "", false
	}
	return id, true
}

// GetCampaignSettings reads one campaign under the connection's own ad account. A PURE READ —
// its failure is always definite, never an unconfirmed write.
//
// Outcomes:
//   - the campaign                                       -> (settings, nil)
//   - a 404, with the ad account then confirmed readable -> (nil, nil)
//   - anything unverifiable                              -> (nil, error)
//
// A campaign 404 alone does NOT prove absence: LinkedIn's error-handling guide says "In some
// cases (Ads, for example), a 404 error is returned when attempting to access a restricted API"
// (https://learn.microsoft.com/en-us/linkedin/shared/api-guide/concepts/error-handling). So a 404
// is followed by ONE confirming read of the account (GET adAccounts/{id}) whose echoed id must be
// the connection's; anything else leaves the absence unproven — the Reddit and X adopters' rule.
//
// "Unverifiable" also covers: a body identityjson refuses or that does not decode, an echoed id
// that is not the one asked for, a malformed account/campaignGroup URN, and a status outside the
// eight the schema documents.
func (c *Client) GetCampaignSettings(ctx context.Context, campaignID string) (*CampaignSettings, error) {
	if err := c.validateCredentialShape(); err != nil {
		return nil, err
	}
	if err := ValidateCampaignID(campaignID); err != nil {
		return nil, err
	}
	accountID, err := c.resolveAccountID("")
	if err != nil {
		return nil, fmt.Errorf("linkedin: %w", err)
	}
	raw, err := c.doEntityGET(ctx, fmt.Sprintf("adAccounts/%s/adCampaigns/%s", accountID, campaignID))
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			if cerr := c.confirmAccountReadable(ctx, accountID); cerr != nil {
				return nil, fmt.Errorf("linkedin: read campaign %s: the campaign read answered 404, but the ad account could not be confirmed readable, so the absence is not proven: %w", campaignID, cerr)
			}
			return nil, nil
		}
		return nil, fmt.Errorf("linkedin: read campaign %s: %w", campaignID, err)
	}
	if err := identityjson.Check(raw); err != nil {
		return nil, fmt.Errorf("linkedin: read campaign %s: %w", campaignID, err)
	}
	var wire campaignSettingsWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		// The decode error is dropped: it can name a JSON kind and a field, and the caller has
		// no use for either.
		return nil, fmt.Errorf("linkedin: read campaign %s: the response is not a campaign object", campaignID)
	}
	if got := wire.ID.String(); got != campaignID {
		return nil, fmt.Errorf("linkedin: read campaign %s: the response does not describe the requested campaign, so nothing in it can be trusted", campaignID)
	}
	out := &CampaignSettings{
		CampaignID:             campaignID,
		Name:                   wire.Name,
		Status:                 wire.Status,
		ObjectiveType:          wire.ObjectiveType,
		OptimizationTargetType: wire.OptimizationTargetType,
		CostType:               wire.CostType,
	}
	if wire.Account != nil {
		id, ok := urnTrailingID(*wire.Account, "sponsoredAccount")
		if !ok {
			return nil, fmt.Errorf("linkedin: read campaign %s: the campaign's account is not a sponsoredAccount URN", campaignID)
		}
		out.AccountID = id
	}
	if wire.CampaignGroup != nil {
		id, ok := urnTrailingID(*wire.CampaignGroup, "sponsoredCampaignGroup")
		if !ok {
			return nil, fmt.Errorf("linkedin: read campaign %s: the campaign's campaignGroup is not a sponsoredCampaignGroup URN", campaignID)
		}
		out.CampaignGroupID = id
	}
	if wire.Status != nil && !knownStatus(*wire.Status) {
		return nil, fmt.Errorf("linkedin: read campaign %s: the campaign reports a status this client does not recognise", campaignID)
	}
	if u := wire.UnitCost; u != nil {
		out.UnitCostMicros, out.UnitCostUnparseable = parseDecimalMicros(u.Amount)
		if u.CurrencyCode != nil {
			out.UnitCostCurrency = *u.CurrencyCode
		}
	}
	if b := wire.DailyBudget; b != nil {
		var bad bool
		out.DailyBudgetMicros, bad = parseDecimalMicros(b.Amount)
		out.BudgetUnparseable = out.BudgetUnparseable || bad
		if b.CurrencyCode != nil {
			out.BudgetCurrency = *b.CurrencyCode
		}
	}
	if b := wire.TotalBudget; b != nil {
		var bad bool
		out.TotalBudgetMicros, bad = parseDecimalMicros(b.Amount)
		out.BudgetUnparseable = out.BudgetUnparseable || bad
		if out.BudgetCurrency == "" && b.CurrencyCode != nil {
			out.BudgetCurrency = *b.CurrencyCode
		}
	}
	if rs := wire.RunSchedule; rs != nil {
		if rs.Start != nil {
			t := time.UnixMilli(*rs.Start).UTC()
			out.RunStart = &t
		}
		if rs.End != nil {
			t := time.UnixMilli(*rs.End).UTC()
			out.RunEnd = &t
		}
	}
	return out, nil
}

// confirmAccountReadable is the proof behind a campaign 404: one read of the connection's own
// ad account, which must answer 2xx with an identity-checked body whose id is that account.
func (c *Client) confirmAccountReadable(ctx context.Context, accountID string) error {
	raw, err := c.doEntityGET(ctx, "adAccounts/"+accountID)
	if err != nil {
		return err
	}
	if err := identityjson.Check(raw); err != nil {
		return err
	}
	var acct struct {
		ID flexibleID `json:"id"`
	}
	if err := json.Unmarshal(raw, &acct); err != nil {
		return fmt.Errorf("the ad account read is not an account object")
	}
	if acct.ID.String() != accountID {
		return fmt.Errorf("the ad account read does not describe the connection's account")
	}
	return nil
}

// doEntityGET performs ONE authenticated GET of a single entity and returns its raw 2xx body.
// It exists because doRequest decodes into the search envelope (and refuses a body without
// `elements`), which a single-entity GET does not carry, and because the identity check needs
// the raw bytes. Not retried: a read failure is reported, never papered over.
//
// A non-2xx is an *apiError (its body never surfaced), a 401 the shared credentialsExpiredError,
// and a transport or read failure a transportError — all on a GET, so none is ever an
// unconfirmed write.
func (c *Client) doEntityGET(ctx context.Context, path string) ([]byte, error) {
	if !pathValidRE.MatchString(path) || strings.Contains(path, "..") {
		return nil, fmt.Errorf("invalid LinkedIn API path: %q", path)
	}
	u, err := url.Parse(c.baseURL + "/" + path)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	attemptCtx, cancel, token, err := c.authorizedAttempt(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("LinkedIn-Version", c.apiVersion)
	req.Header.Set("X-RestLi-Protocol-Version", "2.0.0")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &transportError{Method: http.MethodGet, Path: path, Err: redactHTTPDoError(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, &transportError{Method: http.MethodGet, Path: path, Err: redactBodyReadError(err)}
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, &transportError{Method: http.MethodGet, Path: path, Err: fmt.Errorf("response exceeds %d bytes", maxResponseBytes)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if ce := c.expiredCredentialsError(resp.StatusCode, string(body), http.MethodGet); ce != nil {
			return nil, ce
		}
		return nil, &apiError{StatusCode: resp.StatusCode, Method: http.MethodGet, Path: path}
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, &transportError{Method: http.MethodGet, Path: path, Err: fmt.Errorf("empty response body")}
	}
	return body, nil
}

// AdoptedCampaignResult is the provenance blob an ADOPTED campaign's row records: the account it
// was verified under (what linkedInCreationAccountID reads back), the Campaign Manager link, and
// Adopted — never a campaign group, creative count or any other child the create path records.
func AdoptedCampaignResult(accountID, campaignID, name, step string) *CampaignResult {
	return &CampaignResult{
		Platform:     "linkedin-ads",
		CampaignName: name,
		CampaignID:   campaignID,
		AccountID:    accountID,
		LinkedInURL:  campaignManagerURL(accountID, campaignID),
		Steps:        []string{step},
		Adopted:      true,
	}
}
