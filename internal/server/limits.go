package server

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/ijcodelabs/vigil/internal/fleet"
	"github.com/ijcodelabs/vigil/internal/promwire"
)

const (
	signalMetrics = "metrics"
	signalLogs    = "logs"
	// limitWindow is the time a limit is averaged over: an agent may send a
	// minute's worth at once, for example after it was cut off for a while.
	limitWindow = 60.0
)

// ingestLimiter keeps one allowance per agent and signal. An agent over its
// limit is told to try again (429): it keeps the batch, falls behind, and
// once its spool is full drops its own oldest data. The server and the
// stores never take more than the limit from any one agent.
type ingestLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens   float64
	at       time.Time
	rejected uint64    // batches refused since the server started
	last     time.Time // when the last one was refused
}

// allow reports whether agent may send n more units of signal at rate
// units a second. A batch is taken whole while any allowance is left, so a
// single large batch cannot be stuck forever.
func (l *ingestLimiter) allow(agent, signal string, n, rate int, now time.Time) bool {
	if rate <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := agent + "\x00" + signal
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(rate) * limitWindow, at: now}
		l.buckets[key] = b
	}
	b.tokens = min(float64(rate)*limitWindow, b.tokens+now.Sub(b.at).Seconds()*float64(rate))
	b.at = now
	if b.tokens <= 0 {
		b.rejected++
		b.last = now
		return false
	}
	b.tokens -= float64(n)
	return true
}

// rejected returns how many batches of an agent were refused, and whether
// any was within the last few minutes.
func (l *ingestLimiter) state(agent, signal string, now time.Time) (rejected uint64, recent bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.buckets[agent+"\x00"+signal]; b != nil {
		return b.rejected, !b.last.IsZero() && now.Sub(b.last) < 5*time.Minute
	}
	return 0, false
}

// overLimit answers an agent that sends more than it may.
func overLimit(w http.ResponseWriter, what string, rate int) {
	w.Header().Set("Retry-After", "30")
	http.Error(w, fmt.Sprintf("this agent sends more than the server's limit of %d %s per second; the batch was not stored, send it again later", rate, what),
		http.StatusTooManyRequests)
}

// bounded lets only so many batches be stored at once. What is over is told
// to come back, which agents do on their own.
func (s *Server) bounded(next http.HandlerFunc) http.HandlerFunc {
	if s.slots == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
			next(w, r)
		default:
			s.busy.Add(1)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "the server is busy, try again", http.StatusServiceUnavailable)
		}
	}
}

// tokenOwner finds the agent a token belongs to. Every batch of every agent
// asks, so the answer is remembered; the memory is dropped whenever a token
// is issued or revoked.
func (s *Server) tokenOwner(hash string) string {
	s.tokenMu.Lock()
	owner, ok := s.tokens[hash]
	s.tokenMu.Unlock()
	if ok {
		return owner
	}
	owner, _ = s.store.AgentForToken(hash)
	if owner != "" {
		s.tokenMu.Lock()
		if len(s.tokens) > 100000 {
			s.tokens = map[string]string{}
		}
		s.tokens[hash] = owner
		s.tokenMu.Unlock()
	}
	return owner
}

func (s *Server) forgetTokens() {
	s.tokenMu.Lock()
	s.tokens = map[string]string{}
	s.tokenMu.Unlock()
}

// signalSeries is a limit on how many different series an agent creates,
// next to how many samples it sends: a program that puts an id or a time
// into a label makes a new series with every sample, which costs the
// metrics store memory however few samples there are.
const signalSeries = "series"

// seriesWindow is how long the series an agent wrote are remembered. Within
// it, writing the same series again costs nothing; after it, the count
// starts over.
const seriesWindow = time.Hour

type seriesSeen struct {
	since time.Time
	ids   map[uint64]struct{}
}

// seriesLimiter counts the different series each limited agent wrote in
// the current window. Only agents with a limit are tracked.
type seriesLimiter struct {
	mu     sync.Mutex
	agents map[string]*seriesSeen
}

// allow reports whether an agent may write a batch with these series
// without going over max different series in the window. A batch is taken
// or refused whole, so what is stored stays consistent.
func (l *seriesLimiter) allow(agent string, ids []uint64, max int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.agents[agent]
	if s == nil || now.Sub(s.since) >= seriesWindow {
		s = &seriesSeen{since: now, ids: map[uint64]struct{}{}}
		l.agents[agent] = s
	}
	fresh := 0
	for _, id := range ids {
		if _, ok := s.ids[id]; !ok {
			fresh++
		}
	}
	if len(s.ids)+fresh > max {
		return false
	}
	for _, id := range ids {
		s.ids[id] = struct{}{}
	}
	return true
}

// refused records a batch refused for a limit kept outside the limiter's
// own buckets, so it shows with the others on the Fleet page and in
// vigil_agent_limited_batches_total.
func (l *ingestLimiter) refused(agent, signal string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := agent + "\x00" + signal
	b := l.buckets[key]
	if b == nil {
		b = &bucket{at: now}
		l.buckets[key] = b
	}
	b.rejected++
	b.last = now
}

// seriesLimit is the series_per_hour of an agent: its own, or for an agent
// of another kind the default for those. 0 means no limit.
func (s *Server) seriesLimit(id string) int {
	max := s.cfg.Limits.For(id).SeriesPerHour
	if max == 0 {
		s.fleet.mu.Lock()
		attrs := s.fleet.attrs[id]
		s.fleet.mu.Unlock()
		if attrs != nil && !fleet.OwnKind(attrs) {
			max = s.cfg.Limits.OtherKindsSeriesPerHour
		}
	}
	return max
}

// seriesIDs returns a hash of every different series in a write request.
func seriesIDs(body []byte) ([]uint64, error) {
	samples, err := promwire.Decode(body)
	if err != nil {
		return nil, err
	}
	seen := map[uint64]struct{}{}
	out := []uint64{}
	keys := []string{}
	for _, smp := range samples {
		keys = keys[:0]
		for k := range smp.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		h := fnv.New64a()
		h.Write([]byte(smp.Name))
		for _, k := range keys {
			h.Write([]byte{0xff})
			h.Write([]byte(k))
			h.Write([]byte{0xfe})
			h.Write([]byte(smp.Labels[k]))
		}
		id := h.Sum64()
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out, nil
}
