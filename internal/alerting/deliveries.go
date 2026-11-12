package alerting

import (
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/redact"
)

// Whether notifications reach anyone is the one thing an alerting system
// cannot report through itself alone: a contact that keeps failing goes
// unnoticed until someone misses an alert. Every attempt of every
// integration is counted here, with the last error, so the server can show
// it and alert on it through the contacts that still work.

// Delivery is how one integration of a contact (its first Telegram, its
// second webhook, …) has done since the server started.
type Delivery struct {
	Receiver    string    `json:"receiver"`
	Integration string    `json:"integration"` // telegram, webhook, slack, …
	Index       int       `json:"index"`
	Sent        uint64    `json:"sent"`
	Failed      uint64    `json:"failed"`
	LastOK      time.Time `json:"last_ok,omitzero"`
	LastFailure time.Time `json:"last_failure,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	// Check is the scheduled check (checks.go); nil while checks are off.
	Check *CheckResult `json:"check,omitempty"`
}

type deliveries struct {
	mu sync.Mutex
	m  map[string]*Delivery
}

// An error can carry the address it failed to reach, and an address can
// carry a token (a bot token in Telegram's path, a key in a webhook's
// query). Only the scheme and host are kept, and the rest is masked like
// a log line.
var (
	errorMask, _ = redact.New(config.Redact{})
	urlRe        = regexp.MustCompile(`https?://[^\s"']+`)
	hostRe       = regexp.MustCompile(`^(https?://)(?:[^@/]*@)?([^/?#]+)`)
)

func cleanError(err error) string {
	s := urlRe.ReplaceAllStringFunc(err.Error(), func(u string) string {
		m := hostRe.FindStringSubmatch(u)
		if m == nil {
			return "[address]"
		}
		return m[1] + m[2] + "/…"
	})
	s = errorMask.String(s)
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func (d *deliveries) record(receiver, integration string, index int, err error, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.m == nil {
		d.m = map[string]*Delivery{}
	}
	key := fmt.Sprintf("%s\x00%s\x00%d", receiver, integration, index)
	x := d.m[key]
	if x == nil {
		x = &Delivery{Receiver: receiver, Integration: integration, Index: index}
		d.m[key] = x
	}
	if err == nil {
		x.Sent++
		x.LastOK = now
		return
	}
	x.Failed++
	x.LastFailure = now
	x.LastError = cleanError(err)
}

// Deliveries lists every integration of the contacts in use, and any other
// that has tried to send since the server started, by contact. One that has
// not sent anything yet is listed with zeros, so it can be tested.
func (a *Alertmanager) Deliveries() []Delivery {
	a.sent.mu.Lock()
	defer a.sent.mu.Unlock()
	out := make([]Delivery, 0, len(a.sent.m))
	seen := map[string]bool{}
	for k, x := range a.sent.m {
		out = append(out, *x)
		seen[k] = true
	}
	if rt := a.routing.Load(); rt != nil {
		for name, integrations := range rt.receivers {
			for _, in := range integrations {
				t := Target{Receiver: name, Integration: in.Name(), Index: in.Index()}
				if !seen[t.key()] {
					out = append(out, Delivery{Receiver: name, Integration: in.Name(), Index: in.Index()})
				}
			}
		}
	}
	if a.checksOn.Load() {
		checked := a.checkResults()
		for i := range out {
			k := Target{out[i].Receiver, out[i].Integration, out[i].Index}.key()
			r, ok := checked[k]
			if !ok { // after a restart, or a contact added since the last round
				r = CheckResult{State: CheckPending, LastProof: a.checks.proofOf(k)}
			}
			out[i].Check = &r
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Receiver != out[j].Receiver {
			return out[i].Receiver < out[j].Receiver
		}
		if out[i].Integration != out[j].Integration {
			return out[i].Integration < out[j].Integration
		}
		return out[i].Index < out[j].Index
	})
	return out
}
