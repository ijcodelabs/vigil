package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/ijcodelabs/vigil/internal/model"
	"github.com/ijcodelabs/vigil/internal/promwire"
)

// Notification delivery: how each contact's integrations did since the
// server started (alerting/deliveries.go), for the Contacts tab, and as
// vigil_notifications_total{receiver, integration, index, result} so the
// built-in NotificationsFailing rule can tell the contacts that still work.

func (s *Server) deliveryRoutes(mux *http.ServeMux) {
	// Admins only, like the configuration the errors come from.
	mux.HandleFunc("GET /api/v1/alerting/deliveries", s.admin(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.am.Deliveries())
	}))
}

func (s *Server) deliveryLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			list := s.am.Deliveries()
			if len(list) == 0 {
				continue
			}
			samples := make([]model.Sample, 0, 2*len(list))
			for _, d := range list {
				l := map[string]string{"receiver": d.Receiver, "integration": d.Integration, "index": strconv.Itoa(d.Index)}
				ok, failed := map[string]string{"result": "ok"}, map[string]string{"result": "failed"}
				for k, v := range l {
					ok[k], failed[k] = v, v
				}
				samples = append(samples,
					model.Sample{Name: "vigil_notifications_total", Labels: ok, Value: float64(d.Sent), TS: now.UnixMilli()},
					model.Sample{Name: "vigil_notifications_total", Labels: failed, Value: float64(d.Failed), TS: now.UnixMilli()})
			}
			if err := s.forward(ctx, "", promwire.Encode(samples)); err != nil && ctx.Err() == nil {
				s.log.Warn("writing notification counts failed", "err", err)
			}
		}
	}
}
