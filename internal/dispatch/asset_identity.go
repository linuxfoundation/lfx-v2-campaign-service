// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

// UsesAssetReserver reports whether the dispatcher charges exactly the reserver a. The
// container's wiring test asserts IDENTITY with it, not merely a non-nil binding: the aggregate
// asset-memory bound only holds when every asset-holding dispatcher and the signed-asset route
// charge ONE instance, and a dispatcher left on a second reserver still reads as "set".
func (d *MetaDispatcher) UsesAssetReserver(a *AssetReserver) bool { return a != nil && d.assets == a }

// UsesAssetReserver is MetaDispatcher.UsesAssetReserver for the Microsoft dispatcher.
func (d *MicrosoftDispatcher) UsesAssetReserver(a *AssetReserver) bool {
	return a != nil && d.assets == a
}

// UsesAssetReserver is MetaDispatcher.UsesAssetReserver for the X dispatcher.
func (d *TwitterDispatcher) UsesAssetReserver(a *AssetReserver) bool {
	return a != nil && d.assets == a
}
