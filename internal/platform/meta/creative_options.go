// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// DefaultCallToAction is the button every ad carried before metaConfig.callToAction (#550), and
// still carries when it is omitted.
const DefaultCallToAction = "LEARN_MORE"

// callToActionTypes are the call-to-action buttons a website-click event ad may carry: values of
// the Ad Creative Link Data `call_to_action.type` enum (Marketing API v23,
// AdCreativeLinkDataCallToAction.Type) that send the click to the ad's link. The enum has many more
// (app installs, messaging, shopping); those do not fit a link to an event page, so they are not
// offered. REGISTER_NOW is not a value of the enum; SIGN_UP and BUY_TICKETS are the registration
// buttons.
var callToActionTypes = []string{
	"LEARN_MORE", "SIGN_UP", "BUY_TICKETS", "APPLY_NOW", "BOOK_NOW", "SUBSCRIBE", "DOWNLOAD", "WATCH_MORE", "CONTACT_US",
}

// DefaultConversionEvent is the promoted object's custom_event_type for the conversions objective
// when metaConfig.conversionEvent is omitted — the value every conversions campaign carried
// before #550.
const DefaultConversionEvent = "PURCHASE"

// conversionEvents are the promoted-object `custom_event_type` values (Ad Promoted Object,
// Marketing API v23) an event campaign optimizes for: a ticket purchase, a free registration, a
// lead, a CFP or scholarship application, a newsletter subscription.
var conversionEvents = []string{"PURCHASE", "COMPLETE_REGISTRATION", "LEAD", "SUBMIT_APPLICATION", "SUBSCRIBE"}

// creativeOptionInvalid builds a refusal of one of these two fields. Like publishIdentityInvalid
// it wraps domain.ErrCreateConfigInvalid, so the synchronous create path answers 400, and is a
// domain.PreflightRefusal, so the job reports it verbatim.
func creativeOptionInvalid(field, format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: metaConfig.%s: %s", domain.ErrCreateConfigInvalid, field, fmt.Sprintf(format, args...)))
}

// NormalizeCallToAction returns the call-to-action type to send: DefaultCallToAction for an empty
// or blank value, else the upper-cased value when it is in the allow-list. Anything else is a
// refusal raised before any request.
func NormalizeCallToAction(cta string) (string, error) {
	v := strings.ToUpper(strings.TrimSpace(cta))
	if v == "" {
		return DefaultCallToAction, nil
	}
	for _, t := range callToActionTypes {
		if v == t {
			return v, nil
		}
	}
	return "", creativeOptionInvalid("callToAction", "%s is not one of %s", domain.CallerValue(cta), strings.Join(callToActionTypes, ", "))
}

// NormalizeConversionEvent returns the custom_event_type to send for objective (trimmed and
// lower-cased, empty meaning traffic, as CreateCampaign reads it): "" when the event is blank, else
// the upper-cased event when it is in the allow-list and the objective is conversions. An event on
// any other objective would be silently ignored — only conversions promotes a pixel — so it is
// refused rather than dropped.
func NormalizeConversionEvent(objective, event string) (string, error) {
	v := strings.ToUpper(strings.TrimSpace(event))
	if v == "" {
		return "", nil
	}
	if obj := defaultObjective(strings.ToLower(strings.TrimSpace(objective))); obj != "conversions" {
		return "", creativeOptionInvalid("conversionEvent", "applies only to the conversions objective (objective is %s); omit it", domain.CallerValue(obj))
	}
	for _, e := range conversionEvents {
		if v == e {
			return v, nil
		}
	}
	return "", creativeOptionInvalid("conversionEvent", "%s is not one of %s", domain.CallerValue(event), strings.Join(conversionEvents, ", "))
}
