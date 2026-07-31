// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// The orchestrator discovers a toggle-capable dispatcher via a RUNTIME type assertion
// (`d.(service.StatusToggler)`), so a drifting ToggleStatus signature would not fail the
// build — it would silently stop satisfying the interface and every toggle would return
// ErrToggleUnsupported. These compile-time assertions make such a drift a build error.
var (
	_ service.StatusToggler = (*RedditDispatcher)(nil)
	_ service.StatusToggler = (*MetaDispatcher)(nil)
	_ service.StatusToggler = (*LinkedInDispatcher)(nil)
)

// TestStatusTogglerSatisfied is a no-op runtime anchor for the compile-time assertions
// above (a test file so the service import stays test-only, avoiding any production
// dispatch→service dependency).
func TestStatusTogglerSatisfied(t *testing.T) {}
