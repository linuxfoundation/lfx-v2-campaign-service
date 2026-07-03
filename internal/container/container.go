// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package container provides dependency injection for the application.
package container

import (
	"log/slog"

	svc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// Container holds all application dependencies.
// Add your service implementations and infrastructure clients here.
type Container struct {
	Config  *config.Config
	Service svc.Service
}

// NewContainer creates and wires all application dependencies.
func NewContainer(cfg *config.Config) (*Container, error) {
	slog.Info("initializing dependency container")

	// TODO: initialize infrastructure clients (auth, NATS, database, etc.) and
	// pass them to the campaign service. As dependencies are added, fold their
	// health into service.CampaignService.ServiceReady.
	campaignService := service.NewCampaignService()

	slog.Info("dependency container initialized")
	return &Container{
		Config:  cfg,
		Service: campaignService,
	}, nil
}

// Close releases any resources held by the container (e.g., drain NATS connections).
func (c *Container) Close() error {
	// TODO: close any open connections here.
	return nil
}
