// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"fmt"
	"os"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// redditPixelListEnabled reports whether this deployment opted in to the pixel picker read
// (#549 gap 10). Read per call, like REDDIT_CUSTOM_AUDIENCES_ENABLED, so it flips without a
// rebuild. See constants.EnvRedditPixelListEnabled for why it is off.
func redditPixelListEnabled() bool {
	return os.Getenv(constants.EnvRedditPixelListEnabled) == "true"
}

// ListRedditPixels lists the conversion pixels on the ad account a Reddit create for the project
// uses, for the conversionPixelId picker (#549 gap 10). Credentials resolve as for a create
// (d.creds.resolve: the project's own connection, or the LF system account when the deployment
// forces it), so every pixel listed is on the account a create for this project uses.
//
// called reports whether Reddit was contacted: false for the deployment gate and for a local
// refusal before the request (no connection, an unusable one).
func (d *RedditDispatcher) ListRedditPixels(ctx context.Context, projectID string, platform model.Provider) (_ *model.RedditPixels, called bool, _ error) {
	if !redditPixelListEnabled() {
		return nil, false, fmt.Errorf("the reddit pixel list is disabled (%s is not \"true\") until the shape is exercised against a live ad account: %w",
			constants.EnvRedditPixelListEnabled, domain.ErrTargetingLookupUnsupported)
	}
	client, _, err := d.resolveRedditClientWithCreds(ctx, projectID, platform, d.creds.resolve)
	if err != nil {
		return nil, false, err
	}
	rows, truncated, lerr := client.ListPixels(ctx)
	if lerr != nil {
		return nil, true, lerr
	}
	// make, not var: an account with no pixel is a real, empty answer.
	out := make([]model.RedditPixel, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.RedditPixel{ID: r.ID, Name: r.Name, BusinessID: r.BusinessID})
	}
	return &model.RedditPixels{AccountID: client.AccountID(), Pixels: out, Truncated: truncated}, true, nil
}
