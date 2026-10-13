package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// OwnContact is what you wrote down about a person: your name for them, tags, notes, language and
// time zone. It never comes from WhatsApp, and wame never writes it back there.
type OwnContact struct {
	JID       string    `json:"jid"`
	Name      string    `json:"name"`
	Tags      []string  `json:"tags"`
	Notes     string    `json:"notes"`
	Language  string    `json:"language"`
	TimeZone  string    `json:"time_zone"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

// The fields of an OwnContact a write can name.
const (
	FieldName     = "name"
	FieldTags     = "tags"
	FieldNotes    = "notes"
	FieldLanguage = "language"
	FieldTimeZone = "time_zone"
)

// ContactFields lists the writable fields in a fixed order (also the CSV export's order).
var ContactFields = []string{FieldName, FieldTags, FieldNotes, FieldLanguage, FieldTimeZone}

const (
	// MaxContacts caps wame_contacts, so a script can't fill the database.
	MaxContacts = 100_000
	// MaxContactTags is the most tags one person may have.
	MaxContactTags = 20
)

var (
	ErrContactLimit = errors.New("too many contacts")
	ErrTooManyTags  = errors.New("too many tags")

	contactCap = MaxContacts
)

// EditError names the edit of a batch that broke a limit.
type EditError struct {
	Index int
	Err   error
}

func (e *EditError) Error() string { return fmt.Sprintf("contact %d: %v", e.Index, e.Err) }
func (e *EditError) Unwrap() error { return e.Err }

// Empty reports whether none of the own fields is set; such a row is never stored.
func (c OwnContact) Empty() bool {
	return c.Name == "" && len(c.Tags) == 0 && c.Notes == "" && c.Language == "" && c.TimeZone == ""
}

func (c OwnContact) field(f string) string {
	switch f {
	case FieldName:
		return c.Name
	case FieldTags:
		return encodeTags(c.Tags)
	case FieldNotes:
		return c.Notes
	case FieldLanguage:
		return c.Language
	case FieldTimeZone:
		return c.TimeZone
	}
	return ""
}

func (c *OwnContact) set(f string, from OwnContact) {
	switch f {
	case FieldName:
		c.Name = from.Name
	case FieldTags:
		c.Tags = slices.Clone(from.Tags)
	case FieldNotes:
		c.Notes = from.Notes
	case FieldLanguage:
		c.Language = from.Language
	case FieldTimeZone:
		c.TimeZone = from.TimeZone
	}
}

// ContactEdit is one change to a person's row. Keys are every key the person may be stored under
// (phone JID, @lid, the 9th-digit variant); the first existing row is changed, otherwise a new one
// is stored under Keys[0]. Fields of Set are written (an empty value clears it), the others kept;
// AddTags and RemoveTags then edit the tags, so concurrent tag edits don't overwrite each other.
type ContactEdit struct {
	Keys       []string
	Set        OwnContact
	Fields     []string
	AddTags    []string
	RemoveTags []string
	Actor      string
}

// ContactChange is the row before and after an edit. An Empty side means no row. Changed names the
// fields that differ.
type ContactChange struct {
	Before, After OwnContact
	Changed       []string
}

// ImportResult counts what an import did (or would do, in a dry run), once per person: several lines
// for the same person count as one, by how their row ended up. A row cleared completely counts as
// updated.
type ImportResult struct {
	Added     int `json:"added"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
}

// ContactFilter narrows Contacts: every tag in Tags (AND), and only JIDs when it isn't nil.
type ContactFilter struct {
	Tags []string
	JIDs []string
}

// TagCount is one tag and how many people have it.
type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

func migrateContacts(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS wame_contacts (
	jid        TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	tags       TEXT NOT NULL DEFAULT '',
	notes      TEXT NOT NULL DEFAULT '',
	language   TEXT NOT NULL DEFAULT '',
	time_zone  TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	created_by TEXT NOT NULL,
	updated_by TEXT NOT NULL
);`)
	return err
}

// encodeTags stores tags as ",a,b," (sorted, no duplicates) so one tag matches with instr.
func encodeTags(tags []string) string {
	tags = NormalizeTags(tags)
	if len(tags) == 0 {
		return ""
	}
	return "," + strings.Join(tags, ",") + ","
}

// NormalizeTags sorts tags and drops empty and repeated ones. It never returns nil.
func NormalizeTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t != "" {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func decodeTags(s string) []string { return NormalizeTags(strings.Split(s, ",")) }

const contactCols = `jid, name, tags, notes, language, time_zone, created_at, created_by, updated_at, updated_by`

func scanContact(r scanner) (OwnContact, error) {
	var c OwnContact
	var tags string
	var created, updated int64
	err := r.Scan(&c.JID, &c.Name, &tags, &c.Notes, &c.Language, &c.TimeZone, &created, &c.CreatedBy, &updated, &c.UpdatedBy)
	c.Tags, c.CreatedAt, c.UpdatedAt = decodeTags(tags), time.UnixMilli(created), time.UnixMilli(updated)
	return c, err
}

type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// contactUnder returns the row stored under the first of keys that has one.
func contactUnder(q querier, keys []string) (OwnContact, error) {
	if len(keys) == 0 {
		return OwnContact{}, ErrNotFound
	}
	rows, err := q.Query(`SELECT `+contactCols+` FROM wame_contacts WHERE jid IN (`+placeholders(len(keys))+`)`, anys(keys)...)
	if err != nil {
		return OwnContact{}, err
	}
	defer rows.Close()
	found := map[string]OwnContact{}
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			return OwnContact{}, err
		}
		found[c.JID] = c
	}
	if err := rows.Err(); err != nil {
		return OwnContact{}, err
	}
	for _, k := range keys {
		if c, ok := found[k]; ok {
			return c, nil
		}
	}
	return OwnContact{}, ErrNotFound
}

// ContactsGen changes whenever wame_contacts does: once per successful write (SetContact,
// ImportContacts but not a dry run, DeleteContact, DeleteAllContacts) and when relinkLIDs moves rows
// from an @lid. A copy of the rows kept in memory is current while it hasn't changed.
func (s *Store) ContactsGen() uint64 { return s.contactsGen.Load() }

// Contact returns the row stored under the first of keys that has one, or ErrNotFound.
func (s *Store) Contact(keys ...string) (OwnContact, error) {
	s.relinkLIDs()
	return contactUnder(s.db, keys)
}

// SetContact applies one edit in a transaction and returns the row before and after it.
func (s *Store) SetContact(e ContactEdit) (ContactChange, error) {
	ch, err := s.editContacts([]ContactEdit{e}, false)
	if err != nil {
		return ContactChange{}, err
	}
	return ch[0], nil
}

// ImportContacts applies every edit in one transaction: one failure stores nothing. A dry run
// counts without storing.
func (s *Store) ImportContacts(edits []ContactEdit, dryRun bool) (ImportResult, []ContactChange, error) {
	var res ImportResult
	changes, err := s.editContacts(edits, dryRun)
	if err != nil {
		return res, nil, err
	}
	first, last := map[string]OwnContact{}, map[string]OwnContact{}
	var order []string
	for _, c := range changes {
		if _, ok := first[c.Before.JID]; !ok {
			first[c.Before.JID] = c.Before
			order = append(order, c.Before.JID)
		}
		last[c.Before.JID] = c.After
	}
	for _, jid := range order {
		before, after := first[jid], last[jid]
		switch {
		case !slices.ContainsFunc(ContactFields, func(f string) bool { return before.field(f) != after.field(f) }):
			res.Unchanged++
		case before.Empty():
			res.Added++
		default:
			res.Updated++
		}
	}
	return res, changes, nil
}

func (s *Store) editContacts(edits []ContactEdit, dryRun bool) ([]ContactChange, error) {
	s.relinkLIDs()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Take the write lock before reading, so the rows read can't change before they're written.
	if _, err := tx.Exec(`DELETE FROM wame_contacts WHERE 0`); err != nil {
		return nil, err
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM wame_contacts`).Scan(&count); err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]ContactChange, len(edits))
	for i, e := range edits {
		if len(e.Keys) == 0 {
			return nil, errors.New("contact edit without a key")
		}
		before, err := contactUnder(tx, e.Keys)
		if errors.Is(err, ErrNotFound) {
			before, err = OwnContact{JID: e.Keys[0], Tags: []string{}}, nil
		}
		if err != nil {
			return nil, err
		}
		after := before
		for _, f := range e.Fields {
			after.set(f, e.Set)
		}
		after.Tags = NormalizeTags(slices.DeleteFunc(append(slices.Clone(after.Tags), e.AddTags...),
			func(t string) bool { return slices.Contains(e.RemoveTags, t) }))
		if len(after.Tags) > MaxContactTags {
			return nil, &EditError{i, ErrTooManyTags}
		}
		ch := ContactChange{Before: before, After: after}
		for _, f := range ContactFields {
			if before.field(f) != after.field(f) {
				ch.Changed = append(ch.Changed, f)
			}
		}
		switch {
		case len(ch.Changed) == 0:
		case after.Empty():
			ch.After = OwnContact{JID: before.JID, Tags: []string{}}
			if _, err := tx.Exec(`DELETE FROM wame_contacts WHERE jid = ?`, before.JID); err != nil {
				return nil, err
			}
			if !before.Empty() {
				count--
			}
		default:
			if before.Empty() {
				if count >= contactCap {
					return nil, &EditError{i, ErrContactLimit}
				}
				count++
				after.CreatedAt, after.CreatedBy = now, e.Actor
			}
			after.UpdatedAt, after.UpdatedBy = now, e.Actor
			ch.After = after
			if err := putContact(tx, after); err != nil {
				return nil, err
			}
			s.relinkLater(Message{Chat: after.JID})
		}
		out[i] = ch
	}
	if dryRun {
		return out, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.contactsGen.Add(1)
	return out, nil
}

// putContact stores c, replacing the row under its JID.
func putContact(tx *sql.Tx, c OwnContact) error {
	_, err := tx.Exec(`INSERT INTO wame_contacts (`+contactCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (jid) DO UPDATE SET name = excluded.name, tags = excluded.tags, notes = excluded.notes,
			language = excluded.language, time_zone = excluded.time_zone, created_at = excluded.created_at,
			created_by = excluded.created_by, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		c.JID, c.Name, encodeTags(c.Tags), c.Notes, c.Language, c.TimeZone,
		c.CreatedAt.UnixMilli(), c.CreatedBy, c.UpdatedAt.UnixMilli(), c.UpdatedBy)
	return err
}

// relinkActor is the updated_by of a row merged from an @lid into its phone row.
const relinkActor = "relink"

// relinkContacts moves every own row under a mapped @lid to the phone JID, merging it into the row
// already there, and returns how many it moved. See mergeContacts.
func relinkContacts(tx *sql.Tx) (int, error) {
	rows, err := tx.Query(`SELECT ` + contactCols + `, (SELECT pn FROM whatsmeow_lid_map WHERE lid = replace(jid, '@lid', ''))
		FROM wame_contacts WHERE jid LIKE '%@lid' AND replace(jid, '@lid', '') IN (SELECT lid FROM whatsmeow_lid_map)`)
	if err != nil {
		return 0, err
	}
	var lids []OwnContact
	var pns []string
	for rows.Next() {
		var pn string
		c, err := scanContact(withPN{rows, &pn})
		if err != nil {
			rows.Close()
			return 0, err
		}
		lids, pns = append(lids, c), append(pns, pn+"@s.whatsapp.net")
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := time.Now()
	for i, lid := range lids {
		c, err := contactUnder(tx, pns[i:i+1])
		switch {
		case errors.Is(err, ErrNotFound):
			c, c.JID = lid, pns[i]
		case err != nil:
			return 0, err
		default:
			c = mergeContacts(c, lid, now)
		}
		if _, err := tx.Exec(`DELETE FROM wame_contacts WHERE jid = ?`, lid.JID); err != nil {
			return 0, err
		}
		if err := putContact(tx, c); err != nil {
			return 0, err
		}
	}
	return len(lids), nil
}

type withPN struct {
	scanner
	pn *string
}

func (w withPN) Scan(dest ...any) error { return w.scanner.Scan(append(dest, w.pn)...) }

// mergeContacts merges the row found under an @lid into the phone row: the phone row's name,
// language and time zone win when set; notes are both kept (the phone row's first) when they differ;
// tags are united, the phone row's first, capped at MaxContactTags so the next write isn't refused.
func mergeContacts(phone, lid OwnContact, now time.Time) OwnContact {
	c := phone
	c.Name, c.Language, c.TimeZone = cmp.Or(c.Name, lid.Name), cmp.Or(c.Language, lid.Language), cmp.Or(c.TimeZone, lid.TimeZone)
	if lid.Notes != "" && lid.Notes != c.Notes {
		c.Notes = strings.TrimPrefix(c.Notes+"\n"+lid.Notes, "\n")
	}
	tags := slices.Clone(c.Tags)
	for _, t := range lid.Tags {
		if !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
	}
	c.Tags = NormalizeTags(tags[:min(len(tags), MaxContactTags)])
	if lid.CreatedAt.Before(c.CreatedAt) {
		c.CreatedAt, c.CreatedBy = lid.CreatedAt, lid.CreatedBy
	}
	c.UpdatedAt, c.UpdatedBy = now, relinkActor
	return c
}

// DeleteContact removes the rows under every given key of a person, or returns ErrNotFound when
// there was none.
func (s *Store) DeleteContact(keys ...string) error {
	if len(keys) == 0 {
		return ErrNotFound
	}
	res, err := s.db.Exec(`DELETE FROM wame_contacts WHERE jid IN (`+placeholders(len(keys))+`)`, anys(keys)...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		err = ErrNotFound
	}
	if err == nil {
		s.contactsGen.Add(1)
	}
	return err
}

// DeleteAllContacts empties wame_contacts and returns how many rows there were.
func (s *Store) DeleteAllContacts() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM wame_contacts`)
	if err != nil {
		return 0, err
	}
	s.contactsGen.Add(1)
	return res.RowsAffected()
}

// Contacts lists own rows matching f, by JID.
func (s *Store) Contacts(f ContactFilter) ([]OwnContact, error) {
	s.relinkLIDs()
	where, args := []string{"1"}, []any{}
	for _, t := range NormalizeTags(f.Tags) {
		where = append(where, `instr(tags, ?) > 0`)
		args = append(args, ","+t+",")
	}
	if f.JIDs != nil {
		if len(f.JIDs) == 0 {
			return []OwnContact{}, nil
		}
		where = append(where, `jid IN (`+placeholders(len(f.JIDs))+`)`)
		args = append(args, anys(f.JIDs)...)
	}
	rows, err := s.db.Query(`SELECT `+contactCols+` FROM wame_contacts WHERE `+strings.Join(where, " AND ")+` ORDER BY jid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OwnContact{}
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ContactTags counts the people per tag, by tag.
func (s *Store) ContactTags() ([]TagCount, error) {
	s.relinkLIDs()
	rows, err := s.db.Query(`SELECT tags FROM wame_contacts WHERE tags <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var tags string
		if err := rows.Scan(&tags); err != nil {
			return nil, err
		}
		for _, t := range decodeTags(tags) {
			counts[t]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]TagCount, 0, len(counts))
	for t, n := range counts {
		out = append(out, TagCount{t, n})
	}
	slices.SortFunc(out, func(a, b TagCount) int { return strings.Compare(a.Tag, b.Tag) })
	return out, nil
}
