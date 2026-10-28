// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"sync"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The report-backed reads (the Microsoft keyword and audience reads, and the Microsoft and X
// account monitors) keep the platform's report between requests in a saved-report store: the
// first request submits, a later one collects. The service's store is a database table this
// command must not write, so a run is handed these in-process stores instead. They implement the
// repository contracts exactly — the first pending mark wins, complete and fail are a
// compare-and-set on the pending report id — and live only as long as the process, so a run
// starts from "nothing saved", which is what an account sees the first time its flag is flipped.

// memSnapshot is one key's saved state: the ready and pending halves and the last failure.
type memSnapshot[R, P any] struct {
	ready         *R
	pending       *P
	lastFailure   string
	lastFailureAt *time.Time
}

// memReports is a generic in-process saved-report store. pendingID and readyID name the report
// id of each half, which the compare-and-set compares.
type memReports[K comparable, R, P any] struct {
	mu        sync.Mutex
	rows      map[K]*memSnapshot[R, P]
	pendingID func(*P) string
	readyID   func(*R) string
}

func newMemReports[K comparable, R, P any](pendingID func(*P) string, readyID func(*R) string) *memReports[K, R, P] {
	return &memReports[K, R, P]{rows: map[K]*memSnapshot[R, P]{}, pendingID: pendingID, readyID: readyID}
}

// get returns copies of the halves, or ok=false when nothing was ever saved for key.
func (m *memReports[K, R, P]) get(key K) (ready *R, pending *P, failure string, failureAt *time.Time, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, found := m.rows[key]
	if !found {
		return nil, nil, "", nil, false
	}
	if s.ready != nil {
		r := *s.ready
		ready = &r
	}
	if s.pending != nil {
		p := *s.pending
		pending = &p
	}
	if s.lastFailureAt != nil {
		at := *s.lastFailureAt
		failureAt = &at
	}
	return ready, pending, s.lastFailure, failureAt, true
}

func (m *memReports[K, R, P]) markPending(key K, p P) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rows[key]
	if !ok {
		s = &memSnapshot[R, P]{}
		m.rows[key] = s
	}
	if s.pending != nil {
		return false
	}
	s.pending = &p
	return true
}

func (m *memReports[K, R, P]) complete(key K, r R) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rows[key]
	if !ok || s.pending == nil || m.pendingID(s.pending) != m.readyID(&r) {
		return false
	}
	s.ready, s.pending = &r, nil
	return true
}

func (m *memReports[K, R, P]) fail(key K, reportID, reason string, at time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rows[key]
	if !ok || s.pending == nil || m.pendingID(s.pending) != reportID {
		return false
	}
	s.pending, s.lastFailure, s.lastFailureAt = nil, reason, &at
	return true
}

// memAccountReports is the account monitor's store.
type memAccountReports struct {
	m *memReports[model.AccountReportKey, model.ReadyAccountReport, model.PendingAccountReport]
}

var _ domain.AccountReportRepository = (*memAccountReports)(nil)

func newMemAccountReports() *memAccountReports {
	return &memAccountReports{m: newMemReports[model.AccountReportKey](
		func(p *model.PendingAccountReport) string { return p.ReportID },
		func(r *model.ReadyAccountReport) string { return r.ReportID })}
}

func (s *memAccountReports) GetAccountReport(_ context.Context, key model.AccountReportKey) (*model.AccountReportSnapshot, error) {
	ready, pending, failure, at, ok := s.m.get(key)
	if !ok {
		return nil, domain.ErrNotFound
	}
	return &model.AccountReportSnapshot{Key: key, Ready: ready, Pending: pending, LastFailure: failure, LastFailureAt: at}, nil
}

func (s *memAccountReports) MarkAccountReportPending(_ context.Context, key model.AccountReportKey, p model.PendingAccountReport) (bool, error) {
	return s.m.markPending(key, p), nil
}

func (s *memAccountReports) CompleteAccountReport(_ context.Context, key model.AccountReportKey, r model.ReadyAccountReport) (bool, error) {
	return s.m.complete(key, r), nil
}

func (s *memAccountReports) FailAccountReport(_ context.Context, key model.AccountReportKey, reportID, reason string, at time.Time) (bool, error) {
	return s.m.fail(key, reportID, reason, at), nil
}

// memInsightReports is the keyword and audience reads' store. The two report kinds are kept in
// separate maps, as the service's store keeps them apart by kind.
type memInsightReports struct {
	keywords *memReports[model.InsightReportKey, model.ReadyKeywordReport, model.PendingInsightReport]
	audience *memReports[model.InsightReportKey, model.ReadyAudienceReport, model.PendingInsightReport]
}

var (
	_ domain.KeywordReportRepository  = (*memInsightReports)(nil)
	_ domain.AudienceReportRepository = (*memInsightReports)(nil)
)

func newMemInsightReports() *memInsightReports {
	pid := func(p *model.PendingInsightReport) string { return p.ReportID }
	return &memInsightReports{
		keywords: newMemReports[model.InsightReportKey](pid, func(r *model.ReadyKeywordReport) string { return r.ReportID }),
		audience: newMemReports[model.InsightReportKey](pid, func(r *model.ReadyAudienceReport) string { return r.ReportID }),
	}
}

func (s *memInsightReports) GetKeywordReport(_ context.Context, key model.KeywordReportKey) (*model.KeywordReportSnapshot, error) {
	ready, pending, failure, at, ok := s.keywords.get(key)
	if !ok {
		return nil, domain.ErrNotFound
	}
	return &model.KeywordReportSnapshot{Key: key, Ready: ready, Pending: pending, LastFailure: failure, LastFailureAt: at}, nil
}

func (s *memInsightReports) MarkKeywordReportPending(_ context.Context, key model.KeywordReportKey, p model.PendingKeywordReport) (bool, error) {
	return s.keywords.markPending(key, p), nil
}

func (s *memInsightReports) CompleteKeywordReport(_ context.Context, key model.KeywordReportKey, r model.ReadyKeywordReport) (bool, error) {
	return s.keywords.complete(key, r), nil
}

func (s *memInsightReports) FailKeywordReport(_ context.Context, key model.KeywordReportKey, reportID, reason string, at time.Time) (bool, error) {
	return s.keywords.fail(key, reportID, reason, at), nil
}

func (s *memInsightReports) GetAudienceReport(_ context.Context, key model.InsightReportKey) (*model.AudienceReportSnapshot, error) {
	ready, pending, failure, at, ok := s.audience.get(key)
	if !ok {
		return nil, domain.ErrNotFound
	}
	return &model.AudienceReportSnapshot{Key: key, Ready: ready, Pending: pending, LastFailure: failure, LastFailureAt: at}, nil
}

func (s *memInsightReports) MarkAudienceReportPending(_ context.Context, key model.InsightReportKey, p model.PendingInsightReport) (bool, error) {
	return s.audience.markPending(key, p), nil
}

func (s *memInsightReports) CompleteAudienceReport(_ context.Context, key model.InsightReportKey, r model.ReadyAudienceReport) (bool, error) {
	return s.audience.complete(key, r), nil
}

func (s *memInsightReports) FailAudienceReport(_ context.Context, key model.InsightReportKey, reportID, reason string, at time.Time) (bool, error) {
	return s.audience.fail(key, reportID, reason, at), nil
}
