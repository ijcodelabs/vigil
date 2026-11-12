package server

import (
	"database/sql"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Version history of the whole-document settings: the alerting config
// (contacts and routing) and the rule file. Both are replaced whole on
// every save, so one wrong save, or a script that uploads an old copy,
// used to lose the working text for good. Every save now keeps a version
// with who made it and when, like profiles; going back is putting an old
// version into the editor and saving it again, through the same checks.
//
// The alerting config holds webhook addresses and passwords as written,
// so its history is for admins only, like the config itself.

// keptVersions bounds the history per document; the oldest go first.
const keptVersions = 200

// DocVersion is one saved version, without its text.
type DocVersion struct {
	Version int       `json:"version"`
	Origin  string    `json:"origin"` // server, files, or before-history
	Author  string    `json:"author"`
	Time    time.Time `json:"time"`
	Bytes   int       `json:"bytes"`
}

// originBefore marks the text that was stored before the history existed,
// kept as version 1 the first time the document is saved again.
const originBefore = "before-history"

// SaveDocument stores a whole-document setting and keeps it as a new
// version. Saving the same text as the newest version adds none.
func (s *Store) SaveDocument(key, doc, origin, author string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var last sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(version) FROM setting_versions WHERE key = ?`, key).Scan(&last); err != nil {
		return err
	}
	now := time.Now().Unix()
	next := int(last.Int64) + 1
	if !last.Valid {
		// The first save with history: keep what was there before.
		var old string
		switch err := tx.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&old); {
		case err == nil && old != doc:
			if _, err := tx.Exec(`INSERT INTO setting_versions (key, version, doc, origin, author, time) VALUES (?, 1, ?, ?, '', ?)`,
				key, old, originBefore, now); err != nil {
				return err
			}
			next = 2
		case err != nil && err != sql.ErrNoRows:
			return err
		}
	} else {
		var newest string
		if err := tx.QueryRow(`SELECT doc FROM setting_versions WHERE key = ? AND version = ?`, key, last.Int64).Scan(&newest); err != nil {
			return err
		}
		if newest == doc {
			next = 0 // unchanged: no new version
		}
	}
	if next > 0 {
		if _, err := tx.Exec(`INSERT INTO setting_versions (key, version, doc, origin, author, time) VALUES (?, ?, ?, ?, ?, ?)`,
			key, next, doc, origin, author, now); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM setting_versions WHERE key = ? AND version <= ?`, key, next-keptVersions); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, doc); err != nil {
		return err
	}
	return tx.Commit()
}

// DocumentVersions lists a document's history, newest first.
func (s *Store) DocumentVersions(key string) ([]DocVersion, error) {
	rows, err := s.db.Query(`SELECT version, origin, author, time, length(doc) FROM setting_versions WHERE key = ? ORDER BY version DESC`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DocVersion{}
	for rows.Next() {
		var v DocVersion
		var at int64
		if err := rows.Scan(&v.Version, &v.Origin, &v.Author, &at, &v.Bytes); err != nil {
			return nil, err
		}
		v.Time = time.Unix(at, 0).UTC()
		out = append(out, v)
	}
	return out, rows.Err()
}

// DocumentVersion returns the text of one version.
func (s *Store) DocumentVersion(key string, version int) (string, bool, error) {
	var doc string
	err := s.db.QueryRow(`SELECT doc FROM setting_versions WHERE key = ? AND version = ?`, key, version).Scan(&doc)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return doc, err == nil, err
}

// docVersionRoutes serves a document's history under base: base/versions
// and base/versions/{version}. guard is who may read it.
func (s *Server) docVersionRoutes(mux *http.ServeMux, base, key string, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET "+base+"/versions", guard(func(w http.ResponseWriter, _ *http.Request) {
		list, err := s.store.DocumentVersions(key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, list)
	}))
	mux.HandleFunc("GET "+base+"/versions/{version}", guard(func(w http.ResponseWriter, r *http.Request) {
		v, _ := strconv.Atoi(r.PathValue("version"))
		doc, found, err := s.store.DocumentVersion(key, v)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !found {
			http.Error(w, "no such version", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		io.WriteString(w, doc)
	}))
}
