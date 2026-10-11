package server

import (
	"fmt"
	"net/http"
	"sync"
	"time"
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
