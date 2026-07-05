// Package store persists Incidents in SQLite. Single writer process.
package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

// Incident is one diagnosable episode (see CONTEXT.md).
type Incident struct {
	ID               int64
	Source           string // alertmanager | manual
	GroupKey         string
	AlertNames       string
	Target           string
	Status           string // open | resolved
	CreatedAt        time.Time
	LastSeen         time.Time
	ResolvedAt       *time.Time
	TriageOutput     string
	TriageConfidence float64
	EscalationOutput string // empty when escalation did not run
	ModelUsed        string
	Notified         bool
}

const schema = `
CREATE TABLE IF NOT EXISTS incidents (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	source TEXT NOT NULL,
	group_key TEXT NOT NULL DEFAULT '',
	alert_names TEXT NOT NULL DEFAULT '',
	target TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	created_at TEXT NOT NULL,
	last_seen TEXT NOT NULL,
	resolved_at TEXT,
	triage_output TEXT NOT NULL DEFAULT '',
	triage_confidence REAL NOT NULL DEFAULT 0,
	escalation_output TEXT NOT NULL DEFAULT '',
	model_used TEXT NOT NULL DEFAULT '',
	notified INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_incidents_open_group
	ON incidents (group_key) WHERE status = 'open';
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Create(inc *Incident) error {
	res, err := s.db.Exec(`
		INSERT INTO incidents (source, group_key, alert_names, target, status,
			created_at, last_seen, triage_output, triage_confidence,
			escalation_output, model_used, notified)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inc.Source, inc.GroupKey, inc.AlertNames, inc.Target, inc.Status,
		fmtTime(inc.CreatedAt), fmtTime(inc.LastSeen), inc.TriageOutput,
		inc.TriageConfidence, inc.EscalationOutput, inc.ModelUsed, inc.Notified)
	if err != nil {
		return err
	}
	inc.ID, err = res.LastInsertId()
	return err
}

// FindOpenByGroupKey returns the open incident for an Alertmanager groupKey,
// or nil if none — the dedup key for firing episodes.
func (s *Store) FindOpenByGroupKey(groupKey string) (*Incident, error) {
	row := s.db.QueryRow(`
		SELECT id, source, group_key, alert_names, target, status,
			created_at, last_seen, resolved_at, triage_output,
			triage_confidence, escalation_output, model_used, notified
		FROM incidents WHERE group_key = ? AND status = 'open'`, groupKey)
	inc, err := scanIncident(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return inc, err
}

func (s *Store) Get(id int64) (*Incident, error) {
	row := s.db.QueryRow(`
		SELECT id, source, group_key, alert_names, target, status,
			created_at, last_seen, resolved_at, triage_output,
			triage_confidence, escalation_output, model_used, notified
		FROM incidents WHERE id = ?`, id)
	return scanIncident(row)
}

func (s *Store) TouchLastSeen(id int64, t time.Time) error {
	_, err := s.db.Exec(`UPDATE incidents SET last_seen = ? WHERE id = ?`, fmtTime(t), id)
	return err
}

func (s *Store) Resolve(id int64, t time.Time) error {
	_, err := s.db.Exec(`
		UPDATE incidents SET status = 'resolved', resolved_at = ?, last_seen = ?
		WHERE id = ?`, fmtTime(t), fmtTime(t), id)
	return err
}

// FindRecentMatching returns prior Incidents for Incident Memory: created
// since `since`, sharing the target (when non-empty) or any alertname with
// the incident under diagnosis, excluding that incident itself, newest first,
// capped at limit. Open and resolved rows both qualify.
func (s *Store) FindRecentMatching(target string, alertNames []string, excludeID int64, since time.Time, limit int) ([]*Incident, error) {
	rows, err := s.db.Query(`
		SELECT id, source, group_key, alert_names, target, status,
			created_at, last_seen, resolved_at, triage_output,
			triage_confidence, escalation_output, model_used, notified
		FROM incidents WHERE created_at >= ? AND id != ?
		ORDER BY created_at DESC, id DESC`, fmtTime(since), excludeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Incident
	for rows.Next() && len(out) < limit {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		if matchesMemory(inc, target, alertNames) {
			out = append(out, inc)
		}
	}
	return out, rows.Err()
}

func matchesMemory(inc *Incident, target string, alertNames []string) bool {
	if target != "" && inc.Target == target {
		return true
	}
	if inc.AlertNames == "" {
		return false
	}
	for _, stored := range strings.Split(inc.AlertNames, ",") {
		for _, a := range alertNames {
			if a != "" && a == stored {
				return true
			}
		}
	}
	return false
}

// SetDiagnosis records the triage (and optional escalation) outputs after the
// pipeline ran.
func (s *Store) SetDiagnosis(id int64, triageOutput string, confidence float64, escalationOutput, modelUsed string, notified bool) error {
	_, err := s.db.Exec(`
		UPDATE incidents SET triage_output = ?, triage_confidence = ?,
			escalation_output = ?, model_used = ?, notified = ?
		WHERE id = ?`,
		triageOutput, confidence, escalationOutput, modelUsed, notified, id)
	return err
}

func scanIncident(row interface{ Scan(...any) error }) (*Incident, error) {
	var inc Incident
	var created, lastSeen string
	var resolved sql.NullString
	err := row.Scan(&inc.ID, &inc.Source, &inc.GroupKey, &inc.AlertNames,
		&inc.Target, &inc.Status, &created, &lastSeen, &resolved,
		&inc.TriageOutput, &inc.TriageConfidence, &inc.EscalationOutput,
		&inc.ModelUsed, &inc.Notified)
	if err != nil {
		return nil, err
	}
	inc.CreatedAt, _ = time.Parse(time.RFC3339, created)
	inc.LastSeen, _ = time.Parse(time.RFC3339, lastSeen)
	if resolved.Valid {
		t, _ := time.Parse(time.RFC3339, resolved.String)
		inc.ResolvedAt = &t
	}
	return &inc, nil
}

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }
