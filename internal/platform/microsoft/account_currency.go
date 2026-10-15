// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

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

// currencyCodeISO maps Microsoft's CurrencyCode value set — enum NAMES such as "USDollar", not
// ISO 4217 codes — to the ISO code the monitor publishes. A name not listed here leaves the
// currency unknown: a guessed mapping would mislabel every amount on the account, while an
// absent one only leaves them unlabelled. Names follow Microsoft's CurrencyCode value set
// (Customer Management v13); the few whose ISO code is ambiguous (a redenominated currency the
// value set names once) are left out on purpose.
var currencyCodeISO = map[string]string{
	"AlgerianDinar":                        "DZD",
	"ArgentinePeso":                        "ARS",
	"ArmenianDram":                         "AMD",
	"AustralianDollar":                     "AUD",
	"AzerbaijanianManat":                   "AZN",
	"BahrainiDinar":                        "BHD",
	"BangladeshTaka":                       "BDT",
	"BolivianBoliviano":                    "BOB",
	"BosniaAndHerzegovinaConvertibleMarka": "BAM",
	"BrazilianReal":                        "BRL",
	"BritishPound":                         "GBP",
	"BruneiDollar":                         "BND",
	"BulgarianLev":                         "BGN",
	"CanadianDollar":                       "CAD",
	"ChileanPeso":                          "CLP",
	"ChineseYuanRenminbi":                  "CNY",
	"ColombianPeso":                        "COP",
	"CostaRicanColon":                      "CRC",
	"CroatianKuna":                         "HRK",
	"CzechKoruna":                          "CZK",
	"DanishKrone":                          "DKK",
	"DominicanPeso":                        "DOP",
	"EgyptianPound":                        "EGP",
	"Euro":                                 "EUR",
	"GeorgianLari":                         "GEL",
	"GuatemalanQuetzal":                    "GTQ",
	"HongKongDollar":                       "HKD",
	"HungarianForint":                      "HUF",
	"IcelandicKrona":                       "ISK",
	"IndianRupee":                          "INR",
	"IndonesianRupiah":                     "IDR",
	"IraqiDinar":                           "IQD",
	"IsraeliNewShekel":                     "ILS",
	"JamaicanDollar":                       "JMD",
	"JapaneseYen":                          "JPY",
	"JordanianDinar":                       "JOD",
	"KazakhstaniTenge":                     "KZT",
	"KenyanShilling":                       "KES",
	"KuwaitiDinar":                         "KWD",
	"LebanesePound":                        "LBP",
	"LibyanDinar":                          "LYD",
	"MacaoPataca":                          "MOP",
	"MacedonianDenar":                      "MKD",
	"MalaysianRinggit":                     "MYR",
	"MexicanPeso":                          "MXN",
	"MoroccanDirham":                       "MAD",
	"NewZealandDollar":                     "NZD",
	"NicaraguanCordobaOro":                 "NIO",
	"NigerianNaira":                        "NGN",
	"NorwegianKrone":                       "NOK",
	"OmaniRial":                            "OMR",
	"PakistaniRupee":                       "PKR",
	"PanamanianBalboa":                     "PAB",
	"ParaguayanGuarani":                    "PYG",
	"PeruvianNuevoSol":                     "PEN",
	"PhilippinePeso":                       "PHP",
	"PolishZloty":                          "PLN",
	"QatariRial":                           "QAR",
	"RomanianLeu":                          "RON",
	"RussianRuble":                         "RUB",
	"SaudiRiyal":                           "SAR",
	"SerbianDinar":                         "RSD",
	"SingaporeDollar":                      "SGD",
	"SouthAfricanRand":                     "ZAR",
	"SouthKoreanWon":                       "KRW",
	"SriLankanRupee":                       "LKR",
	"SwedishKrona":                         "SEK",
	"SwissFranc":                           "CHF",
	"TaiwanNewDollar":                      "TWD",
	"ThaiBaht":                             "THB",
	"TrinidadAndTobagoDollar":              "TTD",
	"TunisianDinar":                        "TND",
	"TurkishLira":                          "TRY",
	"UAEDirham":                            "AED",
	"UkraineHryvnia":                       "UAH",
	"UruguayanPeso":                        "UYU",
	"USDollar":                             "USD",
	"VietnameseDong":                       "VND",
	"YemeniRial":                           "YER",
}

// accountQueryResponse is the Account/Query envelope, decoded only for the account's id and
// CurrencyCode. Id is RAW because Microsoft renders a `long` either bare or quoted (see
// rawScalar); Account and CurrencyCode are pointers so absent is told from empty.
type accountQueryResponse struct {
	Account *struct {
		ID           json.RawMessage `json:"Id"`
		CurrencyCode *string         `json:"CurrencyCode"`
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
// the caller treats an error as "currency unknown". A CurrencyCode that is absent, null, or a
// name currencyCodeISO does not list is "" with no error — the account answered, just not with a
// currency this client can name. Errors never carry upstream text.
func (c *Client) AccountCurrency(ctx context.Context) (string, error) {
	if err := ValidateMonitorAccountID(c.account.AccountID); err != nil {
		return "", err
	}
	req := map[string]any{"AccountId": json.Number(c.account.AccountID)}
	// idempotent: a read; a 429 retry cannot change anything.
	body, err := c.doCustomerRequest(ctx, http.MethodPost, "Account/Query", req, true)
	if err != nil {
		return "", fmt.Errorf("read microsoft ad account: %w", err)
	}
	return accountCurrencyFromBody(body, c.account.AccountID)
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
	if resp.Account.CurrencyCode == nil {
		return "", nil
	}
	return model.NormalizeCurrencyCode(currencyCodeISO[*resp.Account.CurrencyCode]), nil
}
