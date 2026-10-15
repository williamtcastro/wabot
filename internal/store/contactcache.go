package store

import (
	"context"
	"database/sql"
	"time"
)

// CachedProfile is what WhatsApp last said about a person, kept so opening a profile never asks it.
// OnWhatsApp is -1 when unknown. PictureID is "" for none and "-" when hidden from this account.
// Times are zero when that part was never fetched.
type CachedProfile struct {
	JID           string
	OnWhatsApp    int
	About         string
	AboutAt       time.Time
	VerifiedName  string
	VerifiedLevel string
	Business      string
	PictureID     string
	PictureURL    string
	LID           string
	InfoAt        time.Time
	PictureAt     time.Time
	BusinessAt    time.Time
	Error         string
	ErrorAt       time.Time
	UpdatedAt     time.Time
}

// PictureHidden is the PictureID of a person who hides their photo from this account.
const PictureHidden = "-"

func migrateContactCache(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS wame_contact_cache (
	jid            TEXT PRIMARY KEY,
	on_whatsapp    INTEGER NOT NULL DEFAULT -1,
	about          TEXT NOT NULL DEFAULT '',
	about_at       INTEGER NOT NULL DEFAULT 0,
	verified_name  TEXT NOT NULL DEFAULT '',
	verified_level TEXT NOT NULL DEFAULT '',
	business       TEXT NOT NULL DEFAULT '',
	picture_id     TEXT NOT NULL DEFAULT '',
	picture_url    TEXT NOT NULL DEFAULT '',
	lid            TEXT NOT NULL DEFAULT '',
	info_at        INTEGER NOT NULL DEFAULT 0,
	picture_at     INTEGER NOT NULL DEFAULT 0,
	business_at    INTEGER NOT NULL DEFAULT 0,
	error          TEXT NOT NULL DEFAULT '',
	error_at       INTEGER NOT NULL DEFAULT 0,
	updated_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS wame_contact_cache_updated ON wame_contact_cache (updated_at);`)
	return err
}

const cacheCols = `jid, on_whatsapp, about, about_at, verified_name, verified_level, business, picture_id, picture_url, lid,
	info_at, picture_at, business_at, error, error_at, updated_at`

func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// PutCachedProfile stores what a refresh learned, replacing the row under c.JID and deleting the
// rows under replaces (the person's other keys), so an older copy can't hide the new one.
func (s *Store) PutCachedProfile(c CachedProfile, replaces ...string) error {
	s.relinkLater(Message{Chat: c.JID})
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if len(replaces) > 0 {
		if _, err := tx.Exec(`DELETE FROM wame_contact_cache WHERE jid <> ? AND jid IN (`+placeholders(len(replaces))+`)`,
			append([]any{c.JID}, anys(replaces)...)...); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`INSERT INTO wame_contact_cache (`+cacheCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (jid) DO UPDATE SET on_whatsapp = excluded.on_whatsapp, about = excluded.about,
			about_at = excluded.about_at, verified_name = excluded.verified_name, verified_level = excluded.verified_level,
			business = excluded.business, picture_id = excluded.picture_id, picture_url = excluded.picture_url,
			lid = excluded.lid, info_at = excluded.info_at, picture_at = excluded.picture_at,
			business_at = excluded.business_at, error = excluded.error, error_at = excluded.error_at,
			updated_at = excluded.updated_at`,
		c.JID, c.OnWhatsApp, c.About, millis(c.AboutAt), c.VerifiedName, c.VerifiedLevel, c.Business, c.PictureID,
		c.PictureURL, c.LID, millis(c.InfoAt), millis(c.PictureAt), millis(c.BusinessAt), c.Error, millis(c.ErrorAt),
		time.Now().UnixMilli())
	if err != nil {
		return err
	}
	return tx.Commit()
}

// CachedProfileOf returns the cache row under the first of keys that has one, or ErrNotFound.
func (s *Store) CachedProfileOf(keys ...string) (CachedProfile, error) {
	if len(keys) == 0 {
		return CachedProfile{}, ErrNotFound
	}
	s.relinkLIDs()
	rows, err := s.db.Query(`SELECT `+cacheCols+` FROM wame_contact_cache WHERE jid IN (`+placeholders(len(keys))+`)`, anys(keys)...)
	if err != nil {
		return CachedProfile{}, err
	}
	defer rows.Close()
	found := map[string]CachedProfile{}
	for rows.Next() {
		var c CachedProfile
		var aboutAt, infoAt, picAt, bizAt, errAt, updated int64
		if err := rows.Scan(&c.JID, &c.OnWhatsApp, &c.About, &aboutAt, &c.VerifiedName, &c.VerifiedLevel, &c.Business,
			&c.PictureID, &c.PictureURL, &c.LID, &infoAt, &picAt, &bizAt, &c.Error, &errAt, &updated); err != nil {
			return CachedProfile{}, err
		}
		c.AboutAt, c.InfoAt, c.PictureAt = fromMillis(aboutAt), fromMillis(infoAt), fromMillis(picAt)
		c.BusinessAt, c.ErrorAt, c.UpdatedAt = fromMillis(bizAt), fromMillis(errAt), fromMillis(updated)
		found[c.JID] = c
	}
	if err := rows.Err(); err != nil {
		return CachedProfile{}, err
	}
	for _, k := range keys {
		if c, ok := found[k]; ok {
			return c, nil
		}
	}
	return CachedProfile{}, ErrNotFound
}

// The Cache* updates below come from WhatsApp notifications. They change an existing row only, so
// strangers' notifications can't grow the table, and report whether one was changed.

// CacheAbout sets the about text of jid when at isn't older than the one stored.
func (s *Store) CacheAbout(jid, about string, at time.Time) (bool, error) {
	return s.updateCache(`UPDATE wame_contact_cache SET about = ?, about_at = ? WHERE jid = ? AND about_at <= ?`,
		about, millis(at), jid, millis(at))
}

// CachePicture sets the picture id of jid ("" when removed) and drops the URL, which belonged to the
// old picture.
func (s *Store) CachePicture(jid, id string) (bool, error) {
	return s.updateCache(`UPDATE wame_contact_cache SET picture_id = ?, picture_url = '' WHERE jid = ?`, id, jid)
}

// CacheVerifiedName sets the verified business name of jid.
func (s *Store) CacheVerifiedName(jid, name string) (bool, error) {
	return s.updateCache(`UPDATE wame_contact_cache SET verified_name = ? WHERE jid = ?`, name, jid)
}

func (s *Store) updateCache(query string, args ...any) (bool, error) {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PruneContactCache deletes cache rows not refreshed since before.
func (s *Store) PruneContactCache(before time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM wame_contact_cache WHERE updated_at < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ClearContactState deletes, in one transaction, everything wame keeps about people: your own
// contacts, the WhatsApp profile cache and group members. It returns how many own contacts there
// were. Messages, group names and the audit log stay. Used on logout.
func (s *Store) ClearContactState() (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM wame_contacts`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	for _, stmt := range []string{`DELETE FROM wame_contact_cache`, `DELETE FROM wame_group_members`} {
		if _, err := tx.Exec(stmt); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.contactsGen.Add(1)
	return n, nil
}

// RelinkLater makes the next read relink rows stored under jid when it is an @lid (WhatsApp may
// just have told whatsmeow its number).
func (s *Store) RelinkLater(jid string) { s.relinkLater(Message{Chat: jid}) }
