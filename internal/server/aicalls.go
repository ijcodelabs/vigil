package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ijcodelabs/vigil/internal/ai"
)

// Every request vigil sends to a language model, whatever asked for it, is
// one row in ai_calls: who, for what, which model, the tokens and what they
// cost, failed calls included (they can cost tokens too). The monthly
// budget, the pause switch and vigil_ai_cost_total all read this ledger, so
// a new AI feature cannot spend outside them. What a feature keeps of its
// own (an explanation's evidence and verdict) stays in its table, linked by
// the call's id.

// aiCall is one request to the model.
type aiCall struct {
	ID        int64
	Created   time.Time
	User      string
	Tool      string // what asked: explain (incidents and log lines), later others; the tool label of the cost metric
	Subject   string // what it was about: incident:<id>, log:<key>
	Provider  string
	Model     string
	Prompt    string // the prompt version
	Usage     ai.Usage
	Cost      float64
	LatencyMS int64
	Error     string // the provider's error, if the call failed
}

func initAICalls(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS ai_calls (id INTEGER PRIMARY KEY AUTOINCREMENT, created INTEGER NOT NULL,
		user TEXT NOT NULL, tool TEXT NOT NULL, subject TEXT NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL, prompt TEXT NOT NULL,
		usage TEXT NOT NULL, cost REAL NOT NULL, latency_ms INTEGER NOT NULL, error TEXT NOT NULL)`)
	if err == nil {
		_, err = db.Exec(`CREATE INDEX IF NOT EXISTS ai_calls_created ON ai_calls (created)`)
	}
	return err
}

// backfillAICalls gives every explanation made before the ledger existed
// its call, so that this month's spend and the cost metric's totals stay
// what they were. It runs at start and finds nothing to do after the first.
func backfillAICalls(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, incident, subject, created, user, provider, model, prompt, usage, cost, latency_ms, error
		FROM explanations WHERE call = 0 ORDER BY id`)
	if err != nil {
		return err
	}
	type old struct {
		id, incident     int64
		c                aiCall
		created          int64
		usage, exSubject string
	}
	var todo []old
	for rows.Next() {
		var o old
		if err := rows.Scan(&o.id, &o.incident, &o.exSubject, &o.created, &o.c.User, &o.c.Provider, &o.c.Model, &o.c.Prompt,
			&o.usage, &o.c.Cost, &o.c.LatencyMS, &o.c.Error); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range todo {
		subject := o.exSubject
		if subject == "" {
			subject = "incident:" + strconv.FormatInt(o.incident, 10)
		}
		res, err := tx.Exec(`INSERT INTO ai_calls (created, user, tool, subject, provider, model, prompt, usage, cost, latency_ms, error)
			VALUES (?, ?, 'explain', ?, ?, ?, ?, ?, ?, ?, ?)`,
			o.created, o.c.User, subject, o.c.Provider, o.c.Model, o.c.Prompt, o.usage, o.c.Cost, o.c.LatencyMS, o.c.Error)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		if _, err := tx.Exec(`UPDATE explanations SET call = ? WHERE id = ?`, id, o.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// callAI sends one request and records it, failed or not. The error is only
// that recording failed; a failed call is in c.Error, with res as far as
// the provider got.
func (s *Server) callAI(ctx context.Context, c aiCall, req ai.Request) (aiCall, ai.Response, error) {
	c.Created, c.Provider, c.Model = time.Now().UTC(), s.ai.Name(), s.ai.Model()
	res, err := s.ai.Complete(ctx, req)
	c.Usage, c.LatencyMS = res.Usage, res.Latency.Milliseconds()
	if res.Model != "" {
		c.Model = res.Model
	}
	c.Cost = s.tokenCost(c.Model, res.Usage)
	if err != nil {
		c.Error = err.Error()
	}
	usage, _ := json.Marshal(c.Usage)
	// Recorded even when the request was cancelled meanwhile: the call may
	// have cost tokens all the same.
	r, err := s.store.db.ExecContext(context.WithoutCancel(ctx), `INSERT INTO ai_calls (created, user, tool, subject, provider, model, prompt, usage, cost, latency_ms, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Created.Unix(), c.User, c.Tool, c.Subject, c.Provider, c.Model, c.Prompt, string(usage), c.Cost, c.LatencyMS, c.Error)
	if err != nil {
		return c, res, err
	}
	c.ID, err = r.LastInsertId()
	return c, res, err
}

// aiReady answers the request itself when nothing may be sent now: no
// provider, paused, or the month's budget used up.
func (s *Server) aiReady(w http.ResponseWriter, ctx context.Context) bool {
	st, err := s.aiState(ctx)
	switch {
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	case !st.Enabled:
		http.Error(w, "no AI provider is set up on this server (ai in the server config)", http.StatusConflict)
	case st.Paused:
		http.Error(w, "AI is paused on this server", http.StatusConflict)
	case st.Spent >= st.Budget:
		http.Error(w, fmt.Sprintf("this month's AI budget (%.2f) is used up (%.2f spent)", st.Budget, st.Spent), http.StatusConflict)
	default:
		return true
	}
	return false
}

// aiSpentSince is what every AI call since t cost.
func (s *Server) aiSpentSince(ctx context.Context, t time.Time) (float64, error) {
	var spent float64
	err := s.store.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost), 0) FROM ai_calls WHERE created >= ?`, t.Unix()).Scan(&spent)
	return spent, err
}

// aiSpendTotal is what AI calls cost so far, by tool and model, for the
// cost metric.
func (s *Server) aiSpendTotal(ctx context.Context) (map[[2]string]float64, error) {
	rows, err := s.store.db.QueryContext(ctx, `SELECT tool, model, SUM(cost) FROM ai_calls GROUP BY tool, model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]string]float64{}
	for rows.Next() {
		var (
			tool, m string
			c       float64
		)
		if err := rows.Scan(&tool, &m, &c); err != nil {
			return nil, err
		}
		out[[2]string{tool, m}] = c
	}
	return out, rows.Err()
}
