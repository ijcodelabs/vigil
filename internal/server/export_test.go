package server

import (
	"context"
	"time"

	"github.com/ijcodelabs/vigil/internal/alerting"
)

// Hooks for the tests in package server_test.

func (s *Server) SendAlerts(ctx context.Context, alerts []alerting.Alert) error {
	return s.am.Send(ctx, alerts)
}

func (s *Server) SyncIncidents(ctx context.Context, now time.Time) { s.syncIncidents(ctx, now) }
