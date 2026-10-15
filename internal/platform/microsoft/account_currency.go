// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// The account monitor's currency (LFXV2-2665). Microsoft's monitor reads — the Campaign
// Management campaign list and the Reporting account report — carry no currency, so the monitor
// asks Customer Management for the account itself: GetAccount, REST POST Account/Query, whose
// AdvertiserAccount carries CurrencyCode.
//
// UNVERIFIED LIVE, like the rest of this package's monitor: the operation and field follow
// Microsoft's published v13 Customer Management reference, but no Microsoft credentials were
// available to exercise them. Every field is optional-and-checked, and anything unexpected leaves
// the currency unknown rather than failing the monitor.

// errAccountCurrencyUntrusted marks an Account/Query answer the monitor will not take a currency
// from. It carries no upstream text.
var errAccountCurrencyUntrusted = errors.New("microsoft-ads: account response could not be trusted")

// microsoftPublishableCurrencies is the subset of Microsoft's v13 CurrencyCode value set (Customer
// Management, learn.microsoft.com/en-us/advertising/customer-management-service/currencycode) that
// the monitor publishes. The value set is three-letter codes, not names, but not all of them are
// CURRENT ISO 4217 codes, so the returned value is published only when it is listed here.
//
// Withheld on purpose — withdrawn or non-ISO codes whose scale or meaning is ambiguous (each was
// replaced by a redenominated currency, so an amount labelled with one could be in either):
// AZM, BGL, BYB, EEK, LTL, LVL, ROL, RUR, SIT, SKK, VEF, YUN, ZWD.
//
// HRK is KEPT although Croatia adopted the euro in 2023: an account Microsoft still reports in HRK
// has its amounts in HRK, so HRK is the accurate label for them, and withholding it would only
// leave them unlabelled.
var microsoftPublishableCurrencies = map[string]struct{}{
	"AED": {}, "ALL": {}, "AMD": {}, "ARS": {}, "AUD": {}, "BHD": {}, "BND": {}, "BOB": {},
	"BRL": {}, "BZD": {}, "CAD": {}, "CHF": {}, "CLP": {}, "CNY": {}, "COP": {}, "CRC": {},
	"CZK": {}, "DKK": {}, "DOP": {}, "DZD": {}, "EGP": {}, "EUR": {}, "GBP": {}, "GEL": {},
	"GTQ": {}, "HKD": {}, "HNL": {}, "HRK": {}, "HUF": {}, "IDR": {}, "ILS": {}, "INR": {},
	"IQD": {}, "IRR": {}, "ISK": {}, "JMD": {}, "JOD": {}, "JPY": {}, "KES": {}, "KGS": {},
	"KRW": {}, "KWD": {}, "KZT": {}, "LBP": {}, "LYD": {}, "MAD": {}, "MKD": {}, "MNT": {},
	"MOP": {}, "MVR": {}, "MXN": {}, "MYR": {}, "NGN": {}, "NIO": {}, "NOK": {}, "NZD": {},
	"OMR": {}, "PAB": {}, "PEN": {}, "PHP": {}, "PKR": {}, "PLN": {}, "PYG": {}, "QAR": {},
	"RON": {}, "SAR": {}, "SEK": {}, "SGD": {}, "SYP": {}, "THB": {}, "TND": {}, "TRY": {},
	"TTD": {}, "TWD": {}, "UAH": {}, "USD": {}, "UYU": {}, "UZS": {}, "VND": {}, "YER": {},
	"ZAR": {},
}

// accountCurrencyCacheFor is how long a recognised currency is reused. An ad account's currency is
// fixed when the account is created and Microsoft does not let it change, so hours cost nothing in
// accuracy and spare one Customer Management call per monitor read.
const accountCurrencyCacheFor = 6 * time.Hour

// accountCurrencyUnrecognisedCacheFor is how long an answer that yielded NO currency (CurrencyCode
// absent, null, not a string, or a code microsoftPublishableCurrencies does not list) is reused:
// short, because it is not a definitive answer — it may reflect a shape this client misreads —
// while still sparing back-to-back monitor reads.
const accountCurrencyUnrecognisedCacheFor = 5 * time.Minute

// accountCurrencyCache is the client's cache of successful AccountCurrency answers, by account id.
type accountCurrencyCache struct {
	mu      sync.Mutex
	entries map[string]accountCurrencyEntry
}

type accountCurrencyEntry struct {
	code    string
	expires time.Time
}

func (c *accountCurrencyCache) get(accountID string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[accountID]
	if !ok || !now.Before(e.expires) {
		return "", false
	}
	return e.code, true
}

// put caches one successful answer: a recognised code for accountCurrencyCacheFor, "" for only
// accountCurrencyUnrecognisedCacheFor.
func (c *accountCurrencyCache) put(accountID, code string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]accountCurrencyEntry{}
	}
	ttl := accountCurrencyCacheFor
	if code == "" {
		ttl = accountCurrencyUnrecognisedCacheFor
	}
	c.entries[accountID] = accountCurrencyEntry{code: code, expires: now.Add(ttl)}
}

// accountQueryResponse is the Account/Query envelope, decoded only for the account's id and
// CurrencyCode. Id is RAW because Microsoft renders a `long` either bare or quoted (see
// rawScalar); CurrencyCode is RAW and decoded on its own, so a value of the wrong JSON type costs
// only the currency; Account is a pointer so absent is told from empty.
type accountQueryResponse struct {
	Account *struct {
		ID           json.RawMessage `json:"Id"`
		CurrencyCode json.RawMessage `json:"CurrencyCode"`
	} `json:"Account"`
}

// AccountCurrency reads the client's ad account's currency as an ISO 4217 code, for the account
// monitor's response. It is ONE identity-checked read:
//
//   - the raw body must pass identityjson (no duplicate keys, case-folded included, and no
//     invalid UTF-8 or lone surrogate that encoding/json would silently replace);
//   - the Account it describes must be the client's own account id.
//
// A body that fails either is an error (errAccountCurrencyUntrusted), as is any request failure;
// the caller treats an error as "currency unknown". CurrencyCode is a three-letter code (the v13
// value set); it is published only when microsoftPublishableCurrencies lists it. One that is
// absent, null, not a string, withdrawn (RUR, YUN, …) or anything else is "" with no error — the
// account answered, just not with a currency this client can name. Errors never carry upstream
// text.
//
// A recognised code is cached on the client for accountCurrencyCacheFor (6h); a "" answer for only
// accountCurrencyUnrecognisedCacheFor (5m); a failure is not cached, so the next monitor read
// tries again. The request is sent with
// idempotent=false so a 429 is returned at once rather than backed off and retried: the currency
// is only a label, and waiting out a throttle would spend the monitor's budget on it.
func (c *Client) AccountCurrency(ctx context.Context) (string, error) {
	if err := ValidateMonitorAccountID(c.account.AccountID); err != nil {
		return "", err
	}
	if code, ok := c.currency.get(c.account.AccountID, c.now()); ok {
		return code, nil
	}
	req := map[string]any{"AccountId": json.Number(c.account.AccountID)}
	// idempotent=false, though this is a read: it is the switch that turns off the 429 retry.
	body, err := c.doCustomerRequest(ctx, http.MethodPost, "Account/Query", req, false)
	if err != nil {
		return "", fmt.Errorf("read microsoft ad account: %w", err)
	}
	code, err := accountCurrencyFromBody(body, c.account.AccountID)
	if err != nil {
		return "", err
	}
	c.currency.put(c.account.AccountID, code, c.now())
	return code, nil
}

// accountCurrencyFromBody is AccountCurrency's decode, split out so its rules are testable on
// bytes alone.
func accountCurrencyFromBody(body []byte, accountID string) (string, error) {
	if err := identityjson.Check(body); err != nil {
		return "", fmt.Errorf("%w: %w", errAccountCurrencyUntrusted, err)
	}
	var resp accountQueryResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("%w: the body could not be decoded", errAccountCurrencyUntrusted)
	}
	if resp.Account == nil {
		return "", fmt.Errorf("%w: no Account object", errAccountCurrencyUntrusted)
	}
	id, isNull, ok := rawScalar(resp.Account.ID)
	if !ok || isNull || id != accountID {
		return "", fmt.Errorf("%w: it does not describe the requested account", errAccountCurrencyUntrusted)
	}
	var code string
	if len(resp.Account.CurrencyCode) == 0 || json.Unmarshal(resp.Account.CurrencyCode, &code) != nil {
		return "", nil
	}
	if _, ok := microsoftPublishableCurrencies[code]; !ok {
		return "", nil
	}
	return model.NormalizeCurrencyCode(code), nil
}
