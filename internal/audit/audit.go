// Package audit writes and reads the append only audit trail.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Entry is one recorded action. Actor is who did it ("customer:alice",
// "operator:bob", "system"), Subject is what it was done to (usually an id).
type Entry struct {
	ID        int64           `json:"id"`
	Actor     string          `json:"actor"`
	Action    string          `json:"action"`
	Subject   string          `json:"subject"`
	Detail    json.RawMessage `json:"detail"`
	CreatedAt time.Time       `json:"created_at"`
}

// Write records an action. Pass the *sql.Tx of the change being audited so
// both commit or roll back together.
func Write(ctx context.Context, q Execer, actor, action, subject string, detail any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	data, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx,
		`INSERT INTO audit_log (actor, action, subject, detail) VALUES ($1, $2, $3, $4)`,
		actor, action, subject, data)
	return err
}

// ForSubject returns every entry about one subject, oldest first.
func ForSubject(ctx context.Context, db *sql.DB, subject string) ([]Entry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, actor, action, subject, detail, created_at
		  FROM audit_log WHERE subject = $1 ORDER BY id`, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.Subject, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
