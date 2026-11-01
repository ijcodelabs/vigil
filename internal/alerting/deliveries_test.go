package alerting

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Every send of every integration is counted, failed ones with their
// error, and the error keeps no address beyond its host.
func TestDeliveries(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			http.Error(w, "no", http.StatusBadRequest) // 4xx: no retry, one attempt
			return
		}
	}))
	defer broken.Close()
	ok := httptest.NewServer(&hook{})
	defer ok.Close()

	am, err := New(t.TempDir(), "http://vigil.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer am.Close()
	if err := am.Apply(fmt.Sprintf(`
route: {receiver: team, group_by: [alertname], group_wait: 0s, group_interval: 1s}
receivers:
  - name: team
    webhook_configs: [{url: %q}, {url: %q}]
`, ok.URL, broken.URL+"/hook?key=not-a-real-key")); err != nil {
		t.Fatal(err)
	}
	if err := am.Send(context.Background(), []Alert{{Labels: map[string]string{"alertname": "DiskFull"}, StartsAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	var list []Delivery
	for range 100 {
		if list = am.Deliveries(); len(list) == 2 && list[0].Sent+list[0].Failed > 0 && list[1].Sent+list[1].Failed > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(list) != 2 {
		t.Fatalf("deliveries: %+v", list)
	}
	good, bad := list[0], list[1]
	if good.Receiver != "team" || good.Integration != "webhook" || good.Index != 0 || good.Sent != 1 || good.Failed != 0 || good.LastOK.IsZero() {
		t.Errorf("the working webhook: %+v", good)
	}
	if bad.Index != 1 || bad.Sent != 0 || bad.Failed == 0 || bad.LastFailure.IsZero() || bad.LastError == "" {
		t.Errorf("the broken webhook: %+v", bad)
	}
	if strings.Contains(bad.LastError, "not-a-real-key") || strings.Contains(bad.LastError, "/hook") {
		t.Errorf("the error keeps the address: %s", bad.LastError)
	}
}

func TestCleanError(t *testing.T) {
	for in, want := range map[string]string{
		`Post "https://api.telegram.org/bot123:abc/sendMessage": dial tcp: timeout`: `Post "https://api.telegram.org/…": dial tcp: timeout`,
		`Post "https://user:pw@hooks.example.com/x?token=y": EOF`:                   `Post "https://hooks.example.com/…": EOF`,
		`unexpected status code 500`:                                                `unexpected status code 500`,
	} {
		if got := cleanError(errors.New(in)); got != want {
			t.Errorf("%s\n got %s\nwant %s", in, got, want)
		}
	}
}
