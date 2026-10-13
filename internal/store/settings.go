package store

import (
	"context"
	"database/sql"
	"time"
)

// Setting is a runtime setting changed from the API, overriding the flag it's named after.
type Setting struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

func migrateSettings(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS wame_settings (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	updated_by TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);`)
	return err
}

// Settings returns every override, by key.
func (s *Store) Settings() (map[string]Setting, error) {
	rows, err := s.db.Query(`SELECT key, value, updated_by, updated_at FROM wame_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Setting{}
	for rows.Next() {
		var st Setting
		var at int64
		if err := rows.Scan(&st.Key, &st.Value, &st.UpdatedBy, &at); err != nil {
			return nil, err
		}
		st.UpdatedAt = time.UnixMilli(at)
		out[st.Key] = st
	}
	return out, rows.Err()
}

// SetSettings writes and deletes overrides in one transaction; a nil value deletes the key.
func (s *Store) SetSettings(changes map[string]*string, by string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for k, v := range changes {
		if v == nil {
			_, err = tx.Exec(`DELETE FROM wame_settings WHERE key = ?`, k)
		} else {
			_, err = tx.Exec(`INSERT INTO wame_settings (key, value, updated_by, updated_at) VALUES (?,?,?,?)
				ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
				k, *v, by, now)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
