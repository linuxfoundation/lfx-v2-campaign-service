// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Mobile operating system targeting (#550, gap 15 remainder): metaConfig.targeting.userOs, sent as
// the ad set's `user_os` (Advanced Targeting reference, Mobile section). The allowed values are the
// reference's format table: `iOS`, `Android`, or one of them with a minimum version
// (`<OS>_ver_<x.x>_and_above`) or a version range (`<OS>_ver_<x.x>_to_<y.y>`, x.x below y.y), the
// versions drawn from the reference's option lists below. Every value is checked here, before any
// request; Meta's own list (`type=adTargetingCategory&class=user_os`) is not read.

// MaxUserOS caps the userOs list: two families with a handful of ranges at most.
const MaxUserOS = 10

// userOSVersions are the version options the reference lists per family.
var userOSVersions = map[string][]string{
	"iOS":     {"2.0", "3.0", "4.0", "4.3", "5.0", "6.0", "7.0", "8.0", "9.0"},
	"Android": {"2.0", "2.1", "2.2", "2.3", "3.0", "3.1", "3.2", "4.0", "4.1", "4.2", "4.3", "4.4", "5.0", "5.1", "6.0", "7.0", "7.1", "8.0"},
}

// userOSRE splits a user_os value, case-insensitively: the family, then an optional version clause.
var userOSRE = regexp.MustCompile(`(?i)^(ios|android)(?:_ver_([0-9]+\.[0-9]+)_(?:(and_above)|to_([0-9]+\.[0-9]+)))?$`)

// userOSValue is one parsed user_os value: its family ("iOS" or "Android") and whether it names a
// version.
type userOSValue struct {
	canonical string
	family    string
	versioned bool
}

// parseUserOS returns the canonical spelling of a user_os value, or ok false when it is not one of
// the reference's formats with listed versions (a range must run from a lower to a higher version).
func parseUserOS(raw string) (userOSValue, bool) {
	m := userOSRE.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return userOSValue{}, false
	}
	family := "iOS"
	if strings.EqualFold(m[1], "android") {
		family = "Android"
	}
	if m[2] == "" {
		return userOSValue{canonical: family, family: family}, true
	}
	versions := userOSVersions[family]
	lo := slices.Index(versions, m[2])
	if lo < 0 {
		return userOSValue{}, false
	}
	if m[3] != "" {
		return userOSValue{canonical: family + "_ver_" + m[2] + "_and_above", family: family, versioned: true}, true
	}
	hi := slices.Index(versions, m[4])
	if hi <= lo {
		return userOSValue{}, false
	}
	return userOSValue{canonical: family + "_ver_" + m[2] + "_to_" + m[4], family: family, versioned: true}, true
}

// normalizeUserOS canonicalizes and de-duplicates the userOs list in order, refusing a value the
// reference does not list, a list over MaxUserOS, and the one combination the reference forbids:
// "You cannot target the minimum version of one platform with the other platform. However you can
// target both platforms without specifying minimal versions of either." So a versioned value makes
// every value of the other family a refusal.
func normalizeUserOS(values []string) ([]string, error) {
	var out []string
	var parsed []userOSValue
	seen := map[string]bool{}
	for _, raw := range values {
		v, ok := parseUserOS(raw)
		if !ok {
			return nil, targetingInvalid("userOs entry %s is not a Meta user_os value (iOS, Android, <OS>_ver_<x.x>_and_above or <OS>_ver_<x.x>_to_<y.y> with a version Meta lists)", domain.CallerValue(raw))
		}
		if seen[v.canonical] {
			continue
		}
		seen[v.canonical] = true
		out = append(out, v.canonical)
		parsed = append(parsed, v)
	}
	if len(out) > MaxUserOS {
		return nil, targetingInvalid("userOs names %d values; at most %d are allowed", len(out), MaxUserOS)
	}
	for _, v := range parsed {
		if !v.versioned {
			continue
		}
		for _, w := range parsed {
			if w.family != v.family {
				return nil, targetingInvalid("userOs %s names a version of %s, so it cannot be combined with %s (Meta cannot target a version of one platform together with the other platform); drop the version or the other platform", v.canonical, v.family, w.canonical)
			}
		}
	}
	return out, nil
}

// CheckUserOS refuses a userOs list the ad set cannot carry with the rest of the create: with
// devicePlatforms [desktop], because an operating system list matches only mobile devices and the
// pair would reach no one (a service rule; Meta documents user_os under mobile targeting); and a
// versioned value on the awareness objective, because Meta's reference says brand awareness "can
// only choose Android or iOS, or all mobile phones", not an OS version. t must be
// NormalizeAudienceTargeting's output; objective is as CreateCampaign reads it (empty is traffic).
func CheckUserOS(t *AudienceTargeting, objective string) error {
	if t == nil || len(t.UserOS) == 0 {
		return nil
	}
	if len(t.DevicePlatforms) == 1 && t.DevicePlatforms[0] == "desktop" {
		return targetingInvalid("userOs targets mobile operating systems, so it cannot be combined with devicePlatforms [desktop]; add mobile or omit userOs")
	}
	if defaultObjective(strings.ToLower(strings.TrimSpace(objective))) == "awareness" {
		for _, v := range t.UserOS {
			if strings.Contains(v, "_ver_") {
				return targetingInvalid("userOs %s names an OS version, which the awareness objective cannot target (Meta allows only iOS or Android there); use iOS or Android", strconv.Quote(v))
			}
		}
	}
	return nil
}
