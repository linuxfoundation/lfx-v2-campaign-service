// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package service — per-provider connection adapters.
//
// Each provider's six methods are thin adapters: they convert the typed Goa
// payload to the generic *model.Connection (plus plaintext credential JSON),
// call the shared core helpers in connection_handler.go, and convert the
// generic result back to the provider's typed Goa result. The repetitive shape
// across providers is intentional; the interesting logic lives in
// connection_handler.go.
package service

import (
	"context"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// ─── GoogleAds ───

func (s *ConnectionService) buildGoogleAdsResult(c *model.Connection) *conn.GoogleAdsConnection {
	r := &conn.GoogleAdsConnection{
		ID:             c.ID,
		ProjectID:      c.ProjectID,
		Label:          optStr(c.Label),
		AccountID:      c.AccountID,
		HasCredentials: c.HasCredentials(),
		Status:         string(c.Status),
		Version:        c.Version,
		Etag:           etag(c.Version),
	}
	r.LoginCustomerID = optStr(c.ProviderConfig["login_customer_id"])
	return r
}

func (s *ConnectionService) CreateGoogleAds(ctx context.Context, p *conn.CreateGoogleAdsPayload) (*conn.GoogleAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderGoogleAds,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"login_customer_id": strVal(cfg.LoginCustomerID),
		},
		CreatedBy: actorFromCtx(ctx),
	}
	created, err := s.createConn(ctx, m, p.Credentials)
	if err != nil {
		return nil, err
	}
	return s.buildGoogleAdsResult(created), nil
}

func (s *ConnectionService) GetGoogleAds(ctx context.Context, p *conn.GetGoogleAdsPayload) (*conn.GoogleAdsConnection, error) {
	c, err := s.getConn(ctx, p.ProjectID, model.ProviderGoogleAds)
	if err != nil {
		return nil, err
	}
	return s.buildGoogleAdsResult(c), nil
}

func (s *ConnectionService) UpdateGoogleAds(ctx context.Context, p *conn.UpdateGoogleAdsPayload) (*conn.GoogleAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderGoogleAds,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"login_customer_id": strVal(cfg.LoginCustomerID),
		},
		UpdatedBy: actorFromCtx(ctx),
	}
	updated, err := s.updateConn(ctx, m, p.IfMatch)
	if err != nil {
		return nil, err
	}
	return s.buildGoogleAdsResult(updated), nil
}

func (s *ConnectionService) DeleteGoogleAds(ctx context.Context, p *conn.DeleteGoogleAdsPayload) error {
	return s.deleteConn(ctx, p.ProjectID, model.ProviderGoogleAds)
}

func (s *ConnectionService) TestGoogleAds(ctx context.Context, p *conn.TestGoogleAdsPayload) (*conn.ConnectionTestResult, error) {
	return s.testConn(ctx, p.ProjectID, model.ProviderGoogleAds)
}

func (s *ConnectionService) SetCredentialGoogleAds(ctx context.Context, p *conn.SetCredentialGoogleAdsPayload) error {
	return s.setCredential(ctx, p.ProjectID, model.ProviderGoogleAds, p.Credentials, actorFromCtx(ctx))
}

// ─── LinkedinAds ───

func (s *ConnectionService) buildLinkedinAdsResult(c *model.Connection) *conn.LinkedinAdsConnection {
	r := &conn.LinkedinAdsConnection{
		ID:             c.ID,
		ProjectID:      c.ProjectID,
		Label:          optStr(c.Label),
		AccountID:      c.AccountID,
		HasCredentials: c.HasCredentials(),
		Status:         string(c.Status),
		Version:        c.Version,
		Etag:           etag(c.Version),
	}
	r.OrgID = optStr(c.ProviderConfig["org_id"])
	return r
}

func (s *ConnectionService) CreateLinkedinAds(ctx context.Context, p *conn.CreateLinkedinAdsPayload) (*conn.LinkedinAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderLinkedInAds,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"org_id": cfg.OrgID,
		},
		CreatedBy: actorFromCtx(ctx),
	}
	created, err := s.createConn(ctx, m, p.Credentials)
	if err != nil {
		return nil, err
	}
	return s.buildLinkedinAdsResult(created), nil
}

func (s *ConnectionService) GetLinkedinAds(ctx context.Context, p *conn.GetLinkedinAdsPayload) (*conn.LinkedinAdsConnection, error) {
	c, err := s.getConn(ctx, p.ProjectID, model.ProviderLinkedInAds)
	if err != nil {
		return nil, err
	}
	return s.buildLinkedinAdsResult(c), nil
}

func (s *ConnectionService) UpdateLinkedinAds(ctx context.Context, p *conn.UpdateLinkedinAdsPayload) (*conn.LinkedinAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderLinkedInAds,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"org_id": cfg.OrgID,
		},
		UpdatedBy: actorFromCtx(ctx),
	}
	updated, err := s.updateConn(ctx, m, p.IfMatch)
	if err != nil {
		return nil, err
	}
	return s.buildLinkedinAdsResult(updated), nil
}

func (s *ConnectionService) DeleteLinkedinAds(ctx context.Context, p *conn.DeleteLinkedinAdsPayload) error {
	return s.deleteConn(ctx, p.ProjectID, model.ProviderLinkedInAds)
}

func (s *ConnectionService) TestLinkedinAds(ctx context.Context, p *conn.TestLinkedinAdsPayload) (*conn.ConnectionTestResult, error) {
	return s.testConn(ctx, p.ProjectID, model.ProviderLinkedInAds)
}

func (s *ConnectionService) SetCredentialLinkedinAds(ctx context.Context, p *conn.SetCredentialLinkedinAdsPayload) error {
	return s.setCredential(ctx, p.ProjectID, model.ProviderLinkedInAds, p.Credentials, actorFromCtx(ctx))
}

// ─── MetaAds ───

func (s *ConnectionService) buildMetaAdsResult(c *model.Connection) *conn.MetaAdsConnection {
	r := &conn.MetaAdsConnection{
		ID:             c.ID,
		ProjectID:      c.ProjectID,
		Label:          optStr(c.Label),
		AccountID:      c.AccountID,
		HasCredentials: c.HasCredentials(),
		Status:         string(c.Status),
		Version:        c.Version,
		Etag:           etag(c.Version),
	}
	r.PageID = optStr(c.ProviderConfig["page_id"])
	r.AppID = optStr(c.ProviderConfig["app_id"])
	return r
}

func (s *ConnectionService) CreateMetaAds(ctx context.Context, p *conn.CreateMetaAdsPayload) (*conn.MetaAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderMetaAds,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"page_id": strVal(cfg.PageID),
			"app_id":  strVal(cfg.AppID),
		},
		CreatedBy: actorFromCtx(ctx),
	}
	created, err := s.createConn(ctx, m, p.Credentials)
	if err != nil {
		return nil, err
	}
	return s.buildMetaAdsResult(created), nil
}

func (s *ConnectionService) GetMetaAds(ctx context.Context, p *conn.GetMetaAdsPayload) (*conn.MetaAdsConnection, error) {
	c, err := s.getConn(ctx, p.ProjectID, model.ProviderMetaAds)
	if err != nil {
		return nil, err
	}
	return s.buildMetaAdsResult(c), nil
}

func (s *ConnectionService) UpdateMetaAds(ctx context.Context, p *conn.UpdateMetaAdsPayload) (*conn.MetaAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderMetaAds,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"page_id": strVal(cfg.PageID),
			"app_id":  strVal(cfg.AppID),
		},
		UpdatedBy: actorFromCtx(ctx),
	}
	updated, err := s.updateConn(ctx, m, p.IfMatch)
	if err != nil {
		return nil, err
	}
	return s.buildMetaAdsResult(updated), nil
}

func (s *ConnectionService) DeleteMetaAds(ctx context.Context, p *conn.DeleteMetaAdsPayload) error {
	return s.deleteConn(ctx, p.ProjectID, model.ProviderMetaAds)
}

func (s *ConnectionService) TestMetaAds(ctx context.Context, p *conn.TestMetaAdsPayload) (*conn.ConnectionTestResult, error) {
	return s.testConn(ctx, p.ProjectID, model.ProviderMetaAds)
}

func (s *ConnectionService) SetCredentialMetaAds(ctx context.Context, p *conn.SetCredentialMetaAdsPayload) error {
	return s.setCredential(ctx, p.ProjectID, model.ProviderMetaAds, p.Credentials, actorFromCtx(ctx))
}

// ─── RedditAds ───

func (s *ConnectionService) buildRedditAdsResult(c *model.Connection) *conn.RedditAdsConnection {
	r := &conn.RedditAdsConnection{
		ID:             c.ID,
		ProjectID:      c.ProjectID,
		Label:          optStr(c.Label),
		AccountID:      c.AccountID,
		HasCredentials: c.HasCredentials(),
		Status:         string(c.Status),
		Version:        c.Version,
		Etag:           etag(c.Version),
	}
	return r
}

func (s *ConnectionService) CreateRedditAds(ctx context.Context, p *conn.CreateRedditAdsPayload) (*conn.RedditAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID:      p.ProjectID,
		Provider:       model.ProviderRedditAds,
		Label:          strVal(cfg.Label),
		AccountID:      cfg.AccountID,
		ProviderConfig: map[string]string{},
		CreatedBy:      actorFromCtx(ctx),
	}
	created, err := s.createConn(ctx, m, p.Credentials)
	if err != nil {
		return nil, err
	}
	return s.buildRedditAdsResult(created), nil
}

func (s *ConnectionService) GetRedditAds(ctx context.Context, p *conn.GetRedditAdsPayload) (*conn.RedditAdsConnection, error) {
	c, err := s.getConn(ctx, p.ProjectID, model.ProviderRedditAds)
	if err != nil {
		return nil, err
	}
	return s.buildRedditAdsResult(c), nil
}

func (s *ConnectionService) UpdateRedditAds(ctx context.Context, p *conn.UpdateRedditAdsPayload) (*conn.RedditAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID:      p.ProjectID,
		Provider:       model.ProviderRedditAds,
		Label:          strVal(cfg.Label),
		AccountID:      cfg.AccountID,
		ProviderConfig: map[string]string{},
		UpdatedBy:      actorFromCtx(ctx),
	}
	updated, err := s.updateConn(ctx, m, p.IfMatch)
	if err != nil {
		return nil, err
	}
	return s.buildRedditAdsResult(updated), nil
}

func (s *ConnectionService) DeleteRedditAds(ctx context.Context, p *conn.DeleteRedditAdsPayload) error {
	return s.deleteConn(ctx, p.ProjectID, model.ProviderRedditAds)
}

func (s *ConnectionService) TestRedditAds(ctx context.Context, p *conn.TestRedditAdsPayload) (*conn.ConnectionTestResult, error) {
	return s.testConn(ctx, p.ProjectID, model.ProviderRedditAds)
}

func (s *ConnectionService) SetCredentialRedditAds(ctx context.Context, p *conn.SetCredentialRedditAdsPayload) error {
	return s.setCredential(ctx, p.ProjectID, model.ProviderRedditAds, p.Credentials, actorFromCtx(ctx))
}

// ─── TwitterAds ───

func (s *ConnectionService) buildTwitterAdsResult(c *model.Connection) *conn.TwitterAdsConnection {
	r := &conn.TwitterAdsConnection{
		ID:             c.ID,
		ProjectID:      c.ProjectID,
		Label:          optStr(c.Label),
		AccountID:      c.AccountID,
		HasCredentials: c.HasCredentials(),
		Status:         string(c.Status),
		Version:        c.Version,
		Etag:           etag(c.Version),
	}
	r.FundingInstrumentID = optStr(c.ProviderConfig["funding_instrument_id"])
	return r
}

func (s *ConnectionService) CreateTwitterAds(ctx context.Context, p *conn.CreateTwitterAdsPayload) (*conn.TwitterAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderTwitterAds,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"funding_instrument_id": strVal(cfg.FundingInstrumentID),
		},
		CreatedBy: actorFromCtx(ctx),
	}
	created, err := s.createConn(ctx, m, p.Credentials)
	if err != nil {
		return nil, err
	}
	return s.buildTwitterAdsResult(created), nil
}

func (s *ConnectionService) GetTwitterAds(ctx context.Context, p *conn.GetTwitterAdsPayload) (*conn.TwitterAdsConnection, error) {
	c, err := s.getConn(ctx, p.ProjectID, model.ProviderTwitterAds)
	if err != nil {
		return nil, err
	}
	return s.buildTwitterAdsResult(c), nil
}

func (s *ConnectionService) UpdateTwitterAds(ctx context.Context, p *conn.UpdateTwitterAdsPayload) (*conn.TwitterAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderTwitterAds,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"funding_instrument_id": strVal(cfg.FundingInstrumentID),
		},
		UpdatedBy: actorFromCtx(ctx),
	}
	updated, err := s.updateConn(ctx, m, p.IfMatch)
	if err != nil {
		return nil, err
	}
	return s.buildTwitterAdsResult(updated), nil
}

func (s *ConnectionService) DeleteTwitterAds(ctx context.Context, p *conn.DeleteTwitterAdsPayload) error {
	return s.deleteConn(ctx, p.ProjectID, model.ProviderTwitterAds)
}

func (s *ConnectionService) TestTwitterAds(ctx context.Context, p *conn.TestTwitterAdsPayload) (*conn.ConnectionTestResult, error) {
	return s.testConn(ctx, p.ProjectID, model.ProviderTwitterAds)
}

func (s *ConnectionService) SetCredentialTwitterAds(ctx context.Context, p *conn.SetCredentialTwitterAdsPayload) error {
	return s.setCredential(ctx, p.ProjectID, model.ProviderTwitterAds, p.Credentials, actorFromCtx(ctx))
}

// ─── MicrosoftAds ───

func (s *ConnectionService) buildMicrosoftAdsResult(c *model.Connection) *conn.MicrosoftAdsConnection {
	r := &conn.MicrosoftAdsConnection{
		ID:             c.ID,
		ProjectID:      c.ProjectID,
		Label:          optStr(c.Label),
		AccountID:      c.AccountID,
		HasCredentials: c.HasCredentials(),
		Status:         string(c.Status),
		Version:        c.Version,
		Etag:           etag(c.Version),
	}
	r.CustomerID = optStr(c.ProviderConfig["customer_id"])
	return r
}

func (s *ConnectionService) CreateMicrosoftAds(ctx context.Context, p *conn.CreateMicrosoftAdsPayload) (*conn.MicrosoftAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderMicrosoftAds,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"customer_id": strVal(cfg.CustomerID),
		},
		CreatedBy: actorFromCtx(ctx),
	}
	created, err := s.createConn(ctx, m, p.Credentials)
	if err != nil {
		return nil, err
	}
	return s.buildMicrosoftAdsResult(created), nil
}

func (s *ConnectionService) GetMicrosoftAds(ctx context.Context, p *conn.GetMicrosoftAdsPayload) (*conn.MicrosoftAdsConnection, error) {
	c, err := s.getConn(ctx, p.ProjectID, model.ProviderMicrosoftAds)
	if err != nil {
		return nil, err
	}
	return s.buildMicrosoftAdsResult(c), nil
}

func (s *ConnectionService) UpdateMicrosoftAds(ctx context.Context, p *conn.UpdateMicrosoftAdsPayload) (*conn.MicrosoftAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderMicrosoftAds,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"customer_id": strVal(cfg.CustomerID),
		},
		UpdatedBy: actorFromCtx(ctx),
	}
	updated, err := s.updateConn(ctx, m, p.IfMatch)
	if err != nil {
		return nil, err
	}
	return s.buildMicrosoftAdsResult(updated), nil
}

func (s *ConnectionService) DeleteMicrosoftAds(ctx context.Context, p *conn.DeleteMicrosoftAdsPayload) error {
	return s.deleteConn(ctx, p.ProjectID, model.ProviderMicrosoftAds)
}

func (s *ConnectionService) TestMicrosoftAds(ctx context.Context, p *conn.TestMicrosoftAdsPayload) (*conn.ConnectionTestResult, error) {
	return s.testConn(ctx, p.ProjectID, model.ProviderMicrosoftAds)
}

func (s *ConnectionService) SetCredentialMicrosoftAds(ctx context.Context, p *conn.SetCredentialMicrosoftAdsPayload) error {
	return s.setCredential(ctx, p.ProjectID, model.ProviderMicrosoftAds, p.Credentials, actorFromCtx(ctx))
}

// ─── Hubspot ───

func (s *ConnectionService) buildHubspotResult(c *model.Connection) *conn.HubspotConnection {
	r := &conn.HubspotConnection{
		ID:             c.ID,
		ProjectID:      c.ProjectID,
		Label:          optStr(c.Label),
		AccountID:      c.AccountID,
		HasCredentials: c.HasCredentials(),
		Status:         string(c.Status),
		Version:        c.Version,
		Etag:           etag(c.Version),
	}
	r.PortalID = optStr(c.ProviderConfig["portal_id"])
	r.SenderEmail = optStr(c.ProviderConfig["sender_email"])
	r.SenderName = optStr(c.ProviderConfig["sender_name"])
	r.BrandKit = optStr(c.ProviderConfig["brand_kit"])
	return r
}

func (s *ConnectionService) CreateHubspot(ctx context.Context, p *conn.CreateHubspotPayload) (*conn.HubspotConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderHubSpot,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"portal_id":    strVal(cfg.PortalID),
			"sender_email": strVal(cfg.SenderEmail),
			"sender_name":  strVal(cfg.SenderName),
			"brand_kit":    strVal(cfg.BrandKit),
		},
		CreatedBy: actorFromCtx(ctx),
	}
	created, err := s.createConn(ctx, m, p.Credentials)
	if err != nil {
		return nil, err
	}
	return s.buildHubspotResult(created), nil
}

func (s *ConnectionService) GetHubspot(ctx context.Context, p *conn.GetHubspotPayload) (*conn.HubspotConnection, error) {
	c, err := s.getConn(ctx, p.ProjectID, model.ProviderHubSpot)
	if err != nil {
		return nil, err
	}
	return s.buildHubspotResult(c), nil
}

func (s *ConnectionService) UpdateHubspot(ctx context.Context, p *conn.UpdateHubspotPayload) (*conn.HubspotConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderHubSpot,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"portal_id":    strVal(cfg.PortalID),
			"sender_email": strVal(cfg.SenderEmail),
			"sender_name":  strVal(cfg.SenderName),
			"brand_kit":    strVal(cfg.BrandKit),
		},
		UpdatedBy: actorFromCtx(ctx),
	}
	updated, err := s.updateConn(ctx, m, p.IfMatch)
	if err != nil {
		return nil, err
	}
	return s.buildHubspotResult(updated), nil
}

func (s *ConnectionService) DeleteHubspot(ctx context.Context, p *conn.DeleteHubspotPayload) error {
	return s.deleteConn(ctx, p.ProjectID, model.ProviderHubSpot)
}

func (s *ConnectionService) TestHubspot(ctx context.Context, p *conn.TestHubspotPayload) (*conn.ConnectionTestResult, error) {
	return s.testConn(ctx, p.ProjectID, model.ProviderHubSpot)
}

func (s *ConnectionService) SetCredentialHubspot(ctx context.Context, p *conn.SetCredentialHubspotPayload) error {
	return s.setCredential(ctx, p.ProjectID, model.ProviderHubSpot, p.Credentials, actorFromCtx(ctx))
}
