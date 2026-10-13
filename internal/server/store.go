package server

import (
	"database/sql"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the server's own state (not telemetry): for now the agent registry.
type Store struct{ db *sql.DB }

type AgentInfo struct {
	ID        string    `json:"id"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Version   string    `json:"version"`
	Addr      string    `json:"addr"`
	Via       string    `json:"via,omitempty"` // relays its data came through, nearest to the agent first
	Up        bool      `json:"up"`
}

func OpenStore(dataDir string) (*Store, error) {
	dsn := "file:" + filepath.Join(dataDir, "vigil.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS agents (
		id         TEXT PRIMARY KEY,
		first_seen INTEGER NOT NULL,
		last_seen  INTEGER NOT NULL,
		version    TEXT NOT NULL,
		addr       TEXT NOT NULL
	)`)
	if err == nil { // added later: the relay an agent's data last came through
		if _, aerr := db.Exec(`ALTER TABLE agents ADD COLUMN via TEXT NOT NULL DEFAULT ''`); aerr != nil && !strings.Contains(aerr.Error(), "duplicate column") {
			err = aerr
		}
	}
	for _, ddl := range []string{
		// settings holds whole documents: the rule file, the alerting config.
		`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS alert_state (rule_key TEXT PRIMARY KEY, data BLOB NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS users (name TEXT PRIMARY KEY, pass_hash TEXT NOT NULL, role TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY, user TEXT NOT NULL, expires INTEGER NOT NULL)`,
		// agent_tokens holds SHA-256 hashes; the tokens themselves are shown once.
		`CREATE TABLE IF NOT EXISTS agent_tokens (agent_id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE)`,
		// agent_configs: target is an agent id, or "*" for the default.
		`CREATE TABLE IF NOT EXISTS agent_configs (target TEXT PRIMARY KEY, yaml TEXT NOT NULL, updated INTEGER NOT NULL)`,
		// What each agent last reported about the config it runs.
		`CREATE TABLE IF NOT EXISTS captures (id TEXT PRIMARY KEY, agent TEXT NOT NULL, rule TEXT NOT NULL,
			severity TEXT NOT NULL, summary TEXT NOT NULL, time INTEGER NOT NULL, from_t INTEGER NOT NULL,
			until_t INTEGER NOT NULL, events INTEGER NOT NULL, size INTEGER NOT NULL)`,
		// A profile is one YAML document; every save keeps the previous ones.
		`CREATE TABLE IF NOT EXISTS profiles (name TEXT PRIMARY KEY, doc TEXT NOT NULL, version INTEGER NOT NULL,
			origin TEXT NOT NULL, author TEXT NOT NULL, updated INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS profile_versions (name TEXT NOT NULL, version INTEGER NOT NULL, doc TEXT NOT NULL,
			origin TEXT NOT NULL, author TEXT NOT NULL, time INTEGER NOT NULL, deleted INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (name, version))`,
		// Labels and facts as each agent last reported them (JSON objects).
		`CREATE TABLE IF NOT EXISTS agent_attrs (agent_id TEXT PRIMARY KEY, labels TEXT NOT NULL, facts TEXT NOT NULL)`,
		// Integrations added or customised on this server; the built-in ones
		// live in the binary.
		// Enrolment tokens: shared, expiring, exchanged by a host for a token
		// of its own. Only the hash is kept.
		`CREATE TABLE IF NOT EXISTS enroll_tokens (id TEXT PRIMARY KEY, name TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE,
			expires INTEGER NOT NULL, max_uses INTEGER NOT NULL, uses INTEGER NOT NULL DEFAULT 0,
			author TEXT NOT NULL, created INTEGER NOT NULL)`,
		// A rollout in progress, one per profile at most, as JSON.
		`CREATE TABLE IF NOT EXISTS rollouts (profile TEXT PRIMARY KEY, state TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS integrations (name TEXT PRIMARY KEY, doc TEXT NOT NULL, author TEXT NOT NULL, updated INTEGER NOT NULL)`,
		// What runs on each agent's host, as it last reported (JSON).
		`CREATE TABLE IF NOT EXISTS agent_inventory (agent_id TEXT PRIMARY KEY, doc TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS dashboards (id TEXT PRIMARY KEY, title TEXT NOT NULL, body TEXT NOT NULL, updated INTEGER NOT NULL)`,
		// Queries saved in Explore. A shared one is everyone's; the others
		// belong to their owner.
		`CREATE TABLE IF NOT EXISTS saved_queries (id TEXT PRIMARY KEY, type TEXT NOT NULL, name TEXT NOT NULL, query TEXT NOT NULL,
			owner TEXT NOT NULL, shared INTEGER NOT NULL, origin TEXT NOT NULL, updated INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS agent_reports (agent_id TEXT PRIMARY KEY, config_hash TEXT NOT NULL, config_error TEXT NOT NULL, at INTEGER NOT NULL)`,
	} {
		if err == nil {
			_, err = db.Exec(ddl)
		}
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Touch records that an agent just pushed.
func (s *Store) Touch(id, version, addr, via string, now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO agents (id, first_seen, last_seen, version, addr, via) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET last_seen = excluded.last_seen, version = excluded.version, addr = excluded.addr, via = excluded.via`,
		id, now.Unix(), now.Unix(), version, addr, via)
	return err
}

// Agents lists every known agent; Up is true when it pushed within timeout.
func (s *Store) Agents(now time.Time, timeout time.Duration) ([]AgentInfo, error) {
	rows, err := s.db.Query(`SELECT id, first_seen, last_seen, version, addr, via FROM agents ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentInfo{}
	for rows.Next() {
		var a AgentInfo
		var first, last int64
		if err := rows.Scan(&a.ID, &first, &last, &a.Version, &a.Addr, &a.Via); err != nil {
			return nil, err
		}
		a.FirstSeen, a.LastSeen = time.Unix(first, 0).UTC(), time.Unix(last, 0).UTC()
		a.Up = now.Sub(a.LastSeen) <= timeout
		out = append(out, a)
	}
	return out, rows.Err()
}

// Setting returns a stored document, or def if it was never saved.
func (s *Store) Setting(key, def string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return def, nil
	}
	return v, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) LoadAlertStates() (map[string][]byte, error) {
	rows, err := s.db.Query(`SELECT rule_key, data FROM alert_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var k string
		var d []byte
		if err := rows.Scan(&k, &d); err != nil {
			return nil, err
		}
		out[k] = d
	}
	return out, rows.Err()
}

func (s *Store) SaveAlertState(key string, data []byte) error {
	_, err := s.db.Exec(`INSERT INTO alert_state (rule_key, data) VALUES (?, ?)
		ON CONFLICT(rule_key) DO UPDATE SET data = excluded.data WHERE data != excluded.data`, key, data)
	return err
}

// PruneAlertStates drops the state of rules that no longer exist.
func (s *Store) PruneAlertStates(keep map[string]bool) error {
	states, err := s.LoadAlertStates()
	if err != nil {
		return err
	}
	for k := range states {
		if !keep[k] {
			if _, err := s.db.Exec(`DELETE FROM alert_state WHERE rule_key = ?`, k); err != nil {
				return err
			}
		}
	}
	return nil
}

// Users and sessions.

type User struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

func (s *Store) Users() ([]User, error) {
	rows, err := s.db.Query(`SELECT name, role FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.Name, &u.Role); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UserAuth returns a user's password hash and role; found is false if absent.
func (s *Store) UserAuth(name string) (hash, role string, found bool, err error) {
	err = s.db.QueryRow(`SELECT pass_hash, role FROM users WHERE name = ?`, name).Scan(&hash, &role)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	return hash, role, err == nil, err
}

// PutUser creates or updates a user. An empty hash keeps the current password.
func (s *Store) PutUser(name, hash, role string) error {
	if hash == "" {
		res, err := s.db.Exec(`UPDATE users SET role = ? WHERE name = ?`, role, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO users (name, pass_hash, role) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET pass_hash = excluded.pass_hash, role = excluded.role`, name, hash, role)
	return err
}

func (s *Store) DeleteUser(name string) error {
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE user = ?`, name); err != nil {
		return err
	}
	// What they saved for themselves goes with them, or the next account
	// of that name would inherit it. What they shared stays.
	if _, err := s.db.Exec(`DELETE FROM saved_queries WHERE owner = ? AND shared = 0`, name); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM users WHERE name = ?`, name)
	return err
}

// DeleteUserSessions signs a user out everywhere, except in the session
// named (the one doing the change), if any.
func (s *Store) DeleteUserSessions(user, except string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE user = ? AND id != ?`, user, except)
	return err
}

func (s *Store) CreateSession(id, user string, expires time.Time) error {
	s.db.Exec(`DELETE FROM sessions WHERE expires < ?`, time.Now().Unix())
	_, err := s.db.Exec(`INSERT INTO sessions (id, user, expires) VALUES (?, ?, ?)`, id, user, expires.Unix())
	return err
}

// SessionUser returns the user a live session belongs to, or "".
func (s *Store) SessionUser(id string) (string, error) {
	var user string
	err := s.db.QueryRow(`SELECT user FROM sessions WHERE id = ? AND expires > ?`, id, time.Now().Unix()).Scan(&user)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return user, err
}

func (s *Store) DeleteSession(id string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// Fleet: per-agent tokens, managed configs, and what agents report back.

func (s *Store) SetAgentToken(agentID, tokenHash string) error {
	_, err := s.db.Exec(`INSERT INTO agent_tokens (agent_id, token_hash) VALUES (?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET token_hash = excluded.token_hash`, agentID, tokenHash)
	return err
}

func (s *Store) DeleteAgentToken(agentID string) error {
	_, err := s.db.Exec(`DELETE FROM agent_tokens WHERE agent_id = ?`, agentID)
	return err
}

// AgentForToken returns the agent a token hash belongs to, or "".
func (s *Store) AgentForToken(tokenHash string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT agent_id FROM agent_tokens WHERE token_hash = ?`, tokenHash).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

type AgentConfig struct {
	Target  string    `json:"target"`
	YAML    string    `json:"yaml,omitempty"`
	Updated time.Time `json:"updated"`
}

func (s *Store) AgentConfigs() ([]AgentConfig, error) {
	rows, err := s.db.Query(`SELECT target, yaml, updated FROM agent_configs ORDER BY target`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentConfig{}
	for rows.Next() {
		var c AgentConfig
		var at int64
		if err := rows.Scan(&c.Target, &c.YAML, &at); err != nil {
			return nil, err
		}
		c.Updated = time.Unix(at, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ReportAgentConfig(agentID, hash, errText string) error {
	_, err := s.db.Exec(`INSERT INTO agent_reports (agent_id, config_hash, config_error, at) VALUES (?, ?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET config_hash = excluded.config_hash, config_error = excluded.config_error, at = excluded.at`,
		agentID, hash, errText, time.Now().Unix())
	return err
}

// fleetMaps loads the small per-agent side tables in one go.
func (s *Store) fleetMaps() (tokens map[string]bool, reports map[string][2]string, err error) {
	tokens, reports = map[string]bool{}, map[string][2]string{}
	rows, err := s.db.Query(`SELECT agent_id FROM agent_tokens`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		tokens[id] = true
	}
	rows.Close()
	rows, err = s.db.Query(`SELECT agent_id, config_hash, config_error FROM agent_reports`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, h, e string
		rows.Scan(&id, &h, &e)
		reports[id] = [2]string{h, e}
	}
	return tokens, reports, rows.Err()
}

// ForgetAgent removes an agent and everything stored about it.
func (s *Store) ForgetAgent(id string) error {
	for _, q := range []string{`DELETE FROM agents WHERE id = ?`, `DELETE FROM agent_tokens WHERE agent_id = ?`,
		`DELETE FROM agent_reports WHERE agent_id = ?`, `DELETE FROM agent_attrs WHERE agent_id = ?`,
		`DELETE FROM agent_inventory WHERE agent_id = ?`} {
		if _, err := s.db.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

// Captures: the index of capture files kept under the data directory.

type CaptureInfo struct {
	ID       string    `json:"id"`
	Agent    string    `json:"agent"`
	Rule     string    `json:"rule"`
	Severity string    `json:"severity"`
	Summary  string    `json:"summary"`
	Time     time.Time `json:"time"`
	From     time.Time `json:"from"`
	Until    time.Time `json:"until"`
	Events   int       `json:"events"`
	Size     int64     `json:"size"`
}

// AddCapture records a capture; it reports false if the id is already taken.
func (s *Store) AddCapture(c CaptureInfo) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO captures (id, agent, rule, severity, summary, time, from_t, until_t, events, size)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.ID, c.Agent, c.Rule, c.Severity, c.Summary,
		c.Time.UnixMilli(), c.From.UnixMilli(), c.Until.UnixMilli(), c.Events, c.Size)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Captures lists captures, newest first.
func (s *Store) Captures() ([]CaptureInfo, error) {
	rows, err := s.db.Query(`SELECT id, agent, rule, severity, summary, time, from_t, until_t, events, size
		FROM captures ORDER BY time DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CaptureInfo{}
	for rows.Next() {
		var c CaptureInfo
		var t, from, until int64
		if err := rows.Scan(&c.ID, &c.Agent, &c.Rule, &c.Severity, &c.Summary, &t, &from, &until, &c.Events, &c.Size); err != nil {
			return nil, err
		}
		c.Time, c.From, c.Until = time.UnixMilli(t).UTC(), time.UnixMilli(from).UTC(), time.UnixMilli(until).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DeleteCapture(id string) error {
	_, err := s.db.Exec(`DELETE FROM captures WHERE id = ?`, id)
	return err
}

// Dashboards: body is the dashboard definition as JSON.

type DashboardRow struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Updated time.Time `json:"updated"`
	Body    string    `json:"-"`
}

func (s *Store) Dashboards() ([]DashboardRow, error) {
	rows, err := s.db.Query(`SELECT id, title, updated FROM dashboards ORDER BY title COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DashboardRow{}
	for rows.Next() {
		var d DashboardRow
		var at int64
		if err := rows.Scan(&d.ID, &d.Title, &at); err != nil {
			return nil, err
		}
		d.Updated = time.Unix(at, 0).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

// Dashboard returns one dashboard's definition; found is false if absent.
func (s *Store) Dashboard(id string) (body string, found bool, err error) {
	err = s.db.QueryRow(`SELECT body FROM dashboards WHERE id = ?`, id).Scan(&body)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return body, err == nil, err
}

func (s *Store) PutDashboard(id, title, body string) error {
	_, err := s.db.Exec(`INSERT INTO dashboards (id, title, body, updated) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET title = excluded.title, body = excluded.body, updated = excluded.updated`,
		id, title, body, time.Now().Unix())
	return err
}

func (s *Store) DeleteDashboard(id string) error {
	_, err := s.db.Exec(`DELETE FROM dashboards WHERE id = ?`, id)
	return err
}

// Profiles.

type ProfileRow struct {
	Name    string    `json:"name"`
	Doc     string    `json:"doc,omitempty"`
	Version int       `json:"version"`
	Origin  string    `json:"origin"` // "server" (UI or API) or "files" (vigil apply)
	Author  string    `json:"author"`
	Updated time.Time `json:"updated"`
	Deleted bool      `json:"deleted,omitempty"` // only in version history
}

func (s *Store) Profiles() ([]ProfileRow, error) {
	rows, err := s.db.Query(`SELECT name, doc, version, origin, author, updated FROM profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProfileRow{}
	for rows.Next() {
		var p ProfileRow
		var at int64
		if err := rows.Scan(&p.Name, &p.Doc, &p.Version, &p.Origin, &p.Author, &at); err != nil {
			return nil, err
		}
		p.Updated = time.Unix(at, 0).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

// nextVersion is one more than the highest version a profile name ever had,
// so numbers are not reused after a delete.
func nextVersion(tx *sql.Tx, name string) (int, error) {
	var v sql.NullInt64
	err := tx.QueryRow(`SELECT MAX(version) FROM profile_versions WHERE name = ?`, name).Scan(&v)
	return int(v.Int64) + 1, err
}

// PutProfile saves a profile as a new version and returns its number.
func (s *Store) PutProfile(name, doc, origin, author string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	v, err := nextVersion(tx, name)
	if err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(`INSERT INTO profile_versions (name, version, doc, origin, author, time) VALUES (?, ?, ?, ?, ?, ?)`,
		name, v, doc, origin, author, now); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO profiles (name, doc, version, origin, author, updated) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET doc = excluded.doc, version = excluded.version, origin = excluded.origin,
		author = excluded.author, updated = excluded.updated`, name, doc, v, origin, author, now); err != nil {
		return 0, err
	}
	return v, tx.Commit()
}

// DeleteProfile removes a profile; the deletion is recorded in its history.
func (s *Store) DeleteProfile(name, author string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM profiles WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	v, err := nextVersion(tx, name)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO profile_versions (name, version, doc, origin, author, time, deleted) VALUES (?, ?, '', 'server', ?, ?, 1)`,
		name, v, author, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// ProfileVersions lists a profile's history, newest first, without the text.
func (s *Store) ProfileVersions(name string) ([]ProfileRow, error) {
	rows, err := s.db.Query(`SELECT version, origin, author, time, deleted FROM profile_versions WHERE name = ? ORDER BY version DESC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProfileRow{}
	for rows.Next() {
		p := ProfileRow{Name: name}
		var at int64
		if err := rows.Scan(&p.Version, &p.Origin, &p.Author, &at, &p.Deleted); err != nil {
			return nil, err
		}
		p.Updated = time.Unix(at, 0).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ProfileVersion(name string, version int) (doc string, found bool, err error) {
	err = s.db.QueryRow(`SELECT doc FROM profile_versions WHERE name = ? AND version = ? AND deleted = 0`, name, version).Scan(&doc)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return doc, err == nil, err
}

// AgentAttrs returns the labels and facts every agent last reported, as
// JSON text keyed by agent id.
func (s *Store) AgentAttrs() (map[string][2]string, error) {
	rows, err := s.db.Query(`SELECT agent_id, labels, facts FROM agent_attrs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]string{}
	for rows.Next() {
		var id, l, f string
		if err := rows.Scan(&id, &l, &f); err != nil {
			return nil, err
		}
		out[id] = [2]string{l, f}
	}
	return out, rows.Err()
}

func (s *Store) SetAgentAttrs(agentID, labels, facts string) error {
	_, err := s.db.Exec(`INSERT INTO agent_attrs (agent_id, labels, facts) VALUES (?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET labels = excluded.labels, facts = excluded.facts`, agentID, labels, facts)
	return err
}

// Custom integrations and agent inventories: small keyed documents.

func (s *Store) docs(table, key string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT ` + key + `, doc FROM ` + table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, d string
		if err := rows.Scan(&k, &d); err != nil {
			return nil, err
		}
		out[k] = d
	}
	return out, rows.Err()
}

func (s *Store) Integrations() (map[string]string, error) { return s.docs("integrations", "name") }
func (s *Store) Inventories() (map[string]string, error) {
	return s.docs("agent_inventory", "agent_id")
}

func (s *Store) PutIntegration(name, doc, author string) error {
	_, err := s.db.Exec(`INSERT INTO integrations (name, doc, author, updated) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET doc = excluded.doc, author = excluded.author, updated = excluded.updated`,
		name, doc, author, time.Now().Unix())
	return err
}

func (s *Store) DeleteIntegration(name string) error {
	_, err := s.db.Exec(`DELETE FROM integrations WHERE name = ?`, name)
	return err
}

func (s *Store) SetInventory(agentID, doc string) error {
	_, err := s.db.Exec(`INSERT INTO agent_inventory (agent_id, doc) VALUES (?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET doc = excluded.doc`, agentID, doc)
	return err
}

// Enrolment tokens.

type EnrollToken struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Expires time.Time `json:"expires"`  // zero: never
	MaxUses int       `json:"max_uses"` // zero: unlimited
	Uses    int       `json:"uses"`
	Author  string    `json:"author"`
	Created time.Time `json:"created"`
}

func (s *Store) CreateEnrollToken(t EnrollToken, tokenHash string) error {
	exp := int64(0)
	if !t.Expires.IsZero() {
		exp = t.Expires.Unix()
	}
	_, err := s.db.Exec(`INSERT INTO enroll_tokens (id, name, token_hash, expires, max_uses, author, created) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, tokenHash, exp, t.MaxUses, t.Author, time.Now().Unix())
	return err
}

func (s *Store) EnrollTokens() ([]EnrollToken, error) {
	rows, err := s.db.Query(`SELECT id, name, expires, max_uses, uses, author, created FROM enroll_tokens ORDER BY created DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EnrollToken{}
	for rows.Next() {
		var t EnrollToken
		var exp, created int64
		if err := rows.Scan(&t.ID, &t.Name, &exp, &t.MaxUses, &t.Uses, &t.Author, &created); err != nil {
			return nil, err
		}
		if exp != 0 {
			t.Expires = time.Unix(exp, 0).UTC()
		}
		t.Created = time.Unix(created, 0).UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteEnrollToken(id string) error {
	_, err := s.db.Exec(`DELETE FROM enroll_tokens WHERE id = ?`, id)
	return err
}

// ErrEnroll explains why an enrolment was refused.
type ErrEnroll string

func (e ErrEnroll) Error() string { return string(e) }

// Enroll exchanges an enrolment token for a token of the agent's own, in
// one transaction: the use is counted only if the agent's token is stored.
func (s *Store) Enroll(enrollHash, agentID, agentTokenHash string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	var exp int64
	var max, uses int
	err = tx.QueryRow(`SELECT id, expires, max_uses, uses FROM enroll_tokens WHERE token_hash = ?`, enrollHash).Scan(&id, &exp, &max, &uses)
	switch {
	case err == sql.ErrNoRows:
		return ErrEnroll("this enrolment token is not known; it may have been revoked")
	case err != nil:
		return err
	case exp != 0 && time.Now().Unix() > exp:
		return ErrEnroll("this enrolment token has expired")
	case max != 0 && uses >= max:
		return ErrEnroll("this enrolment token has been used as often as it allows")
	}
	var existing string
	if err := tx.QueryRow(`SELECT agent_id FROM agent_tokens WHERE agent_id = ?`, agentID).Scan(&existing); err == nil {
		return ErrEnroll("an agent called " + agentID + " is already enrolled; revoke its token on the Fleet page first, or choose another agent id")
	} else if err != sql.ErrNoRows {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO agent_tokens (agent_id, token_hash) VALUES (?, ?)`, agentID, agentTokenHash); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE enroll_tokens SET uses = uses + 1 WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Rollouts in progress, keyed by profile.

func (s *Store) Rollouts() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT profile, state FROM rollouts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, state string
		if err := rows.Scan(&name, &state); err != nil {
			return nil, err
		}
		out[name] = state
	}
	return out, rows.Err()
}

func (s *Store) SetRollout(profile, state string) error {
	_, err := s.db.Exec(`INSERT INTO rollouts (profile, state) VALUES (?, ?)
		ON CONFLICT(profile) DO UPDATE SET state = excluded.state`, profile, state)
	return err
}

func (s *Store) DeleteRollout(profile string) error {
	_, err := s.db.Exec(`DELETE FROM rollouts WHERE profile = ?`, profile)
	return err
}
