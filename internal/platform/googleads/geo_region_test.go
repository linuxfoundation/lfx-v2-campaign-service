// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
)

// geoTargetsWithoutRegion lists the paid-targetable countries that are deliberately NOT
// email-reachable, each with the reason. Empty today: every country Google Ads can target is one
// LF runs events in. An entry here is a decision, not a convenience — adding one means a brief can
// run paid ads in that country but cannot build an email audience for it (#200).
var geoTargetsWithoutRegion = map[string]string{}

// TestEveryGeoTargetCountryHasARegion pins #200: every ISO-2 code in geoTargetConstants must
// resolve, through the same path a UI brief takes (audience.CountryForCode then
// audience.RegionFor), to a marketing region. Before #200 Austria, Belgium, Czechia, Denmark,
// Finland and Norway could run paid ads but failed the email audience build. A country added to
// the Google map without its audience mapping (or an explicit, justified exception above) fails
// here instead of surfacing as a failed build on a live brief.
func TestEveryGeoTargetCountryHasARegion(t *testing.T) {
	t.Parallel()

	for code := range geoTargetConstants {
		if reason, excepted := geoTargetsWithoutRegion[code]; excepted {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s is excepted without a reason; every exception must say why", code)
			}
			continue
		}
		name, ok := audience.CountryForCode(code)
		if !ok {
			t.Errorf("%s is paid-targetable but audience.CountryForCode does not know it, so no email audience can be built for it", code)
			continue
		}
		if _, ok := audience.RegionFor(name); !ok {
			t.Errorf("%s resolves to %q, which has no marketing region", code, name)
		}
	}

	for code := range geoTargetsWithoutRegion {
		if _, ok := geoTargetConstants[code]; !ok {
			t.Errorf("exception %s is not in geoTargetConstants; remove the stale exception", code)
		}
	}
}
