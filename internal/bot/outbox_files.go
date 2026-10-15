package bot

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // image.DecodeConfig for queued photos
	_ "image/png"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"github.com/williamtcastro/wame/internal/store"
)

var (
	maxQueueFiles           = 50        // live files per actor; tests lower it
	maxQueueFileBytes int64 = 500 << 20 // live bytes per actor; tests lower it
)

const (
	defaultQueueFileQuota = 2 << 30 // live bytes in all (queue_files_quota)
	defaultQueueMediaTTL  = 24 * time.Hour
	maxQueueMediaTTL      = 7 * 24 * time.Hour
	maxQueueFileQuota     = 1 << 40
	maxQueueImage         = 16 << 20 // larger photos go as documents
	maxImageSide          = 16384
	maxImagePixels        = 100_000_000
	maxQueueFileName      = 255
	queueFileUnused       = 24 * time.Hour   // a file no item ever used
	queueFileAfterDone    = time.Hour        // after its last item finished
	queueFileSweepAge     = 15 * time.Minute // a file on disk without a row (crash during an upload)
	queueReadyFor         = 10 * time.Minute // an upload kept in memory for the item it was made for
	queueUploadTimeout    = 10 * time.Minute // one stuck upload mustn't hold the queue forever
	maxUploadsPerActor    = 2                // POST /queue/files running at once
	maxUploadsInAll       = 8
	maxZipEntries         = 10000
	queueFilesDir         = "wame-queue-files"
)

var queueFileID = regexp.MustCompile(`^f_[0-9a-f]{32}$`)

// ValidQueueFileID reports whether id looks like a queue file id (f_ + 32 hex digits), so routes
// can answer 404 without a lookup.
func ValidQueueFileID(id string) bool { return queueFileID.MatchString(id) }

// FileTooLargeError is a queue upload over the size limit (HTTP 413).
type FileTooLargeError struct{ Limit int64 }

func (e FileTooLargeError) Error() string {
	return fmt.Sprintf("file is larger than %d MB", e.Limit>>20)
}

// FileQuotaError is a queue upload that would go over a quota (HTTP 413). Used is what is taken
// now, without the new file.
type FileQuotaError struct {
	What  string
	Used  int64
	Limit int64
}

func (e FileQuotaError) Error() string {
	if e.What == "files" {
		return fmt.Sprintf("queue file quota: %d of %d files used", e.Used, e.Limit)
	}
	if e.What == "all files" {
		return fmt.Sprintf("queue file quota: the %s the server keeps for queued files is full", FormatBytes(e.Limit))
	}
	return fmt.Sprintf("queue file quota: %s of %s used by %s", FormatBytes(e.Used), FormatBytes(e.Limit), e.What)
}

// FileTypeError is a queue upload whose content isn't on the allow-list (HTTP 415).
type FileTypeError struct{ Reason string }

func (e FileTypeError) Error() string { return e.Reason }

// FileInUseError is a delete of a file queued items still need (HTTP 409).
type FileInUseError struct{ ID string }

func (e FileInUseError) Error() string {
	return fmt.Sprintf("file %s is used by queued messages; cancel them first", e.ID)
}

// ErrFileStorage is a queue upload the server couldn't write to disk (HTTP 507). The cause is
// logged, never sent.
var ErrFileStorage = errors.New("the server couldn't store the file")

// errFileGone fails an item for good: its file was deleted, or changed on disk.
var errFileGone = BadInput("file is gone")

// errFileUnreadable is a queued file the server can't read (the cause is logged, never shown).
var errFileUnreadable = errors.New("the queued file can't be read")

// FormatBytes shows a size the way queue_files_quota takes it: 2GB, 500MB, 64KB or bytes.
func FormatBytes(n int64) string {
	for _, u := range []struct {
		suffix string
		size   int64
	}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}} {
		if n >= u.size && n%u.size == 0 {
			return strconv.FormatInt(n/u.size, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10) + "B"
}

// ParseBytes reads 2GB, 500MB, 64KB, 1TB (powers of 1024) or a plain number of bytes.
func ParseBytes(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		size   int64
	}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if v, ok := strings.CutSuffix(s, u.suffix); ok {
			s, mult = strings.TrimSpace(v), u.size
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > maxQueueFileQuota/mult {
		return 0, errors.New("must be a size like 2GB or 500MB")
	}
	return n * mult, nil
}

// initQueueFiles sets what the queue's files need: where they live (next to the database), the
// upload function (tests swap it) and the settings' defaults.
func (b *Bot) initQueueFiles(dbPath string) {
	b.dataDir = filepath.Dir(dbPath)
	b.uploadMedia = b.Client.Upload
	b.settings = b.settings.withFileDefaults()
	b.baseSettings = b.baseSettings.withFileDefaults()
}

// withFileDefaults fills in the queue file settings, which have no flags: zero means unset here.
func (s Settings) withFileDefaults() Settings {
	if s.QueueMediaTTL == 0 {
		s.QueueMediaTTL = defaultQueueMediaTTL
	}
	if s.QueueFilesQuota == 0 {
		s.QueueFilesQuota = defaultQueueFileQuota
	}
	return s
}

func (b *Bot) queueDir() string { return filepath.Join(b.dataDir, queueFilesDir) }

// queueFilePath is where a file's bytes live; id is always the server's own (ValidQueueFileID).
func (b *Bot) queueFilePath(id string) string { return filepath.Join(b.queueDir(), id) }

func (b *Bot) makeQueueDir() (string, error) {
	dir := b.queueDir()
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	return dir, nil
}

// AddQueueFile stores an upload for the send queue: at most limit bytes, the type sniffed from
// the bytes (never the client's), written 0600 under a random name. The same bytes from the same
// actor return the file already stored (existed true). name is display metadata only.
func (b *Bot) AddQueueFile(actor, tokenID, name string, r io.Reader, limit int64) (f store.OutboxFile, existed bool, err error) {
	defer func() {
		detail := fmt.Sprintf("%s, %d bytes", f.Kind, f.Size)
		if existed {
			detail += ", already uploaded"
		}
		b.Audit(actor, "queue_file", f.ID, detail, err)
	}()
	release, err := b.startUpload(actor)
	if err != nil {
		return f, false, err
	}
	defer release()
	quota := b.currentSettings().QueueFilesQuota
	// Checked again by the INSERT; this keeps a caller over its quota from writing to disk at all.
	if err := b.quotaLeft(actor, quota); err != nil {
		return f, false, err
	}
	dir, err := b.makeQueueDir()
	if err != nil {
		b.Log.Error("queue file: directory", "err", err)
		return f, false, ErrFileStorage
	}
	f.ID = "f_" + randomHex(16)
	path := filepath.Join(dir, f.ID)
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		b.Log.Error("queue file: create", "err", err)
		return store.OutboxFile{}, false, ErrFileStorage
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(path)
		}
	}()
	h, head := sha256.New(), &headBuffer{}
	n, copyErr := io.Copy(io.MultiWriter(out, h, head), io.LimitReader(r, limit+1))
	written, statErr := out.Stat()
	closeErr := cmp.Or(statErr, out.Close())
	_, diskErr := errors.AsType[*fs.PathError](copyErr)
	switch {
	case diskErr:
		b.Log.Error("queue file: write", "err", copyErr)
		return store.OutboxFile{}, false, ErrFileStorage
	case copyErr != nil:
		if _, tooBig := errors.AsType[*http.MaxBytesError](copyErr); tooBig {
			return store.OutboxFile{}, false, copyErr
		}
		return store.OutboxFile{}, false, BadInput("read upload: %v", copyErr)
	case closeErr != nil:
		b.Log.Error("queue file: write", "err", closeErr)
		return store.OutboxFile{}, false, ErrFileStorage
	case n > limit:
		return store.OutboxFile{}, false, FileTooLargeError{Limit: limit}
	case n == 0:
		return store.OutboxFile{}, false, BadInput("file is empty")
	}
	f.Actor, f.TokenID, f.Size, f.SHA256 = actor, tokenID, n, hex.EncodeToString(h.Sum(nil))
	if f.Mime, f.Kind, f.Name, err = sniffQueueFile(head.b, path, n, name); err != nil {
		return store.OutboxFile{}, false, err
	}
	got, existed, err := b.Store.InsertOutboxFile(f, store.OutboxFileQuota{ActorFiles: maxQueueFiles, ActorBytes: maxQueueFileBytes, TotalBytes: quota})
	if errors.Is(err, store.ErrOutboxFileQuota) {
		return store.OutboxFile{}, false, b.quotaError(actor, n, quota, true)
	}
	if err != nil {
		return store.OutboxFile{}, false, err
	}
	if !existed {
		// A logout or the orphan sweep may have unlinked the file while it was being written.
		if onDisk, serr := os.Stat(path); serr != nil || !os.SameFile(written, onDisk) {
			b.Log.Error("queue file: removed while uploading", "id", got.ID)
			if derr := b.Store.DeleteOutboxFile(got.ID, actor); derr != nil {
				b.Log.Error("queue file: forget", "err", derr)
			}
			return store.OutboxFile{}, false, ErrFileStorage
		}
	}
	keep = !existed
	if existed {
		// The stored copy may be missing (logout or a sweep racing an upload, a restored backup):
		// these are the same bytes, so this copy takes its place.
		if _, serr := os.Stat(b.queueFilePath(got.ID)); errors.Is(serr, fs.ErrNotExist) {
			if rerr := os.Rename(path, b.queueFilePath(got.ID)); rerr != nil {
				b.Log.Error("queue file: restore", "err", rerr)
			}
		}
	}
	return got, existed, nil
}

// startUpload takes one of the upload places (two per actor, eight in all) so parallel uploads
// can't fill the disk before the quota sees them; a full house is a 429.
func (b *Bot) startUpload(actor string) (func(), error) {
	b.outbox.mu.Lock()
	defer b.outbox.mu.Unlock()
	if b.outbox.uploading == nil {
		b.outbox.uploading = map[string]int{}
	}
	total := 0
	for _, n := range b.outbox.uploading {
		total += n
	}
	if total >= maxUploadsInAll || b.outbox.uploading[actor] >= maxUploadsPerActor {
		return nil, RateLimitError{RetryAfter: 5 * time.Second}
	}
	b.outbox.uploading[actor]++
	return func() {
		b.outbox.mu.Lock()
		defer b.outbox.mu.Unlock()
		if b.outbox.uploading[actor]--; b.outbox.uploading[actor] <= 0 {
			delete(b.outbox.uploading, actor)
		}
	}, nil
}

// quotaLeft fails when the actor or the server has no room for another file.
func (b *Bot) quotaLeft(actor string, quota int64) error {
	return b.quotaError(actor, 1, quota, false)
}

// quotaError says which quota a file of size bytes goes over (nil for none when !over). The
// total's usage isn't shown: it would tell a caller about other tokens' uploads.
func (b *Bot) quotaError(actor string, size, quota int64, over bool) error {
	own, err := b.Store.OutboxFileUsage(actor)
	if err != nil {
		return err
	}
	total, err := b.Store.OutboxFileUsage("")
	if err != nil {
		return err
	}
	switch {
	case own.Files >= maxQueueFiles:
		return FileQuotaError{What: "files", Used: int64(own.Files), Limit: int64(maxQueueFiles)}
	case own.Bytes+size > maxQueueFileBytes:
		return FileQuotaError{What: actor, Used: own.Bytes, Limit: maxQueueFileBytes}
	case over || total.Bytes+size > quota:
		return FileQuotaError{What: "all files", Limit: quota}
	}
	return nil
}

// headBuffer keeps the first 512 bytes written, all http.DetectContentType reads.
type headBuffer struct{ b []byte }

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := 512 - len(h.b); room > 0 {
		h.b = append(h.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// queueFileTypes is the v1 allow-list: the sniffed type, its kind and the name extensions that
// may go with it (the first is added to a name without one).
var queueFileTypes = map[string]struct {
	kind string
	exts []string
}{
	"image/jpeg":      {store.FileImage, []string{".jpg", ".jpeg", ".jfif"}},
	"image/png":       {store.FileImage, []string{".png"}},
	"image/webp":      {store.FileImage, []string{".webp"}},
	"application/pdf": {store.FileDocument, []string{".pdf"}},
	"text/plain":      {store.FileDocument, []string{".txt", ".csv"}},
	"application/zip": {store.FileDocument, []string{".docx", ".xlsx", ".pptx", ".odt", ".ods", ".odp"}},
}

// zipTypes are the ZIP-based office formats the allow-list takes, by extension, with the entry
// that marks them.
var zipTypes = map[string]struct{ mime, marker string }{
	".docx": {"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "[Content_Types].xml"},
	".xlsx": {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "[Content_Types].xml"},
	".pptx": {"application/vnd.openxmlformats-officedocument.presentationml.presentation", "[Content_Types].xml"},
	".odt":  {"application/vnd.oasis.opendocument.text", "mimetype"},
	".ods":  {"application/vnd.oasis.opendocument.spreadsheet", "mimetype"},
	".odp":  {"application/vnd.oasis.opendocument.presentation", "mimetype"},
}

// sniffQueueFile decides a file's type from its bytes (head is the first 512, path the whole
// file): the MIME type, image or document, and the display name cleaned and given an extension
// that matches the content.
func sniffQueueFile(head []byte, path string, size int64, name string) (mime, kind, display string, err error) {
	full := http.DetectContentType(head)
	detected, _, _ := strings.Cut(full, ";")
	if detected == "text/plain" && full != "text/plain; charset=utf-8" {
		return "", "", "", FileTypeError{"text files must be UTF-8"}
	}
	t, ok := queueFileTypes[detected]
	if !ok {
		return "", "", "", FileTypeError{fmt.Sprintf("files of type %s can't be queued (images: JPEG, PNG, WebP; documents: PDF, text, CSV, Office)", detected)}
	}
	display = CleanFileName(name)
	ext := strings.ToLower(filepath.Ext(display))
	switch {
	case ext == "" && detected == "application/zip":
		return "", "", "", FileTypeError{"a ZIP file needs a name ending in .docx, .xlsx, .pptx, .odt, .ods or .odp"}
	case ext == "":
		ext = t.exts[0]
		display = strings.TrimSuffix(display, ".") + ext
	case !slices.Contains(t.exts, ext):
		// The extension isn't quoted: it's client text and the error reaches the audit log.
		return "", "", "", FileTypeError{fmt.Sprintf("the file name's extension doesn't match its content (%s)", detected)}
	}
	mime, kind = detected, t.kind
	switch detected {
	case "text/plain":
		if trimmed := bytes.TrimLeft(bytes.TrimPrefix(head, []byte("\xef\xbb\xbf")), " \t\r\n\f"); bytes.HasPrefix(trimmed, []byte("<")) {
			return "", "", "", FileTypeError{"text that looks like markup (HTML, SVG, XML) can't be queued"}
		}
		if ext == ".csv" {
			mime = "text/csv"
		}
	case "application/zip":
		z := zipTypes[ext]
		if err := zipHas(path, z.marker); err != nil {
			return "", "", "", FileTypeError{fmt.Sprintf("not a %s document: %v", strings.TrimPrefix(ext, "."), err)}
		}
		mime = z.mime
	case "image/jpeg", "image/png", "image/webp":
		w, h, err := imageSize(head, path, detected)
		if _, gone := errors.AsType[*fs.PathError](err); gone {
			return "", "", "", ErrFileStorage
		}
		if err != nil {
			return "", "", "", FileTypeError{"not a valid image: " + err.Error()}
		}
		if size > maxQueueImage || w > maxImageSide || h > maxImageSide || w*h > maxImagePixels {
			kind = store.FileDocument
		}
	}
	return mime, kind, display, nil
}

// zipHas reports whether a ZIP's central directory names entry. It walks the directory itself, at
// most maxZipEntries headers, instead of zip.OpenReader, which allocates a header for every entry
// (a crafted 64 MB archive holds hundreds of thousands) and trusts only 16 bits of the count.
func zipHas(path, entry string) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("unreadable archive")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return errors.New("unreadable archive")
	}
	tail := make([]byte, min(info.Size(), 22+65535))
	if _, err := f.ReadAt(tail, info.Size()-int64(len(tail))); err != nil {
		return errors.New("unreadable archive")
	}
	i := bytes.LastIndex(tail, []byte("PK\x05\x06"))
	if i < 0 || len(tail)-i < 22 {
		return errors.New("not a ZIP archive")
	}
	off := int64(binary.LittleEndian.Uint32(tail[i+16:]))
	hdr := make([]byte, 46)
	for range maxZipEntries {
		if _, err := f.ReadAt(hdr, off); err != nil || string(hdr[:4]) != "PK\x01\x02" {
			return fmt.Errorf("%s is missing", entry)
		}
		nameLen := int64(binary.LittleEndian.Uint16(hdr[28:]))
		skip := int64(binary.LittleEndian.Uint16(hdr[30:])) + int64(binary.LittleEndian.Uint16(hdr[32:]))
		if nameLen == int64(len(entry)) {
			name := make([]byte, nameLen)
			if _, err := f.ReadAt(name, off+46); err == nil && string(name) == entry {
				return nil
			}
		}
		off += 46 + nameLen + skip
	}
	return errors.New("too many entries")
}

// imageSize reads an image's dimensions without decoding it.
func imageSize(head []byte, path, mime string) (int, int, error) {
	if mime == "image/webp" {
		return webpSize(head)
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	c, _, err := image.DecodeConfig(f)
	return c.Width, c.Height, err
}

// webpSize reads the canvas size from a WebP header (lossy VP8, lossless VP8L or extended VP8X).
func webpSize(b []byte) (int, int, error) {
	if len(b) < 30 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0, errors.New("bad WebP header")
	}
	le24 := func(p []byte) int { return int(p[0]) | int(p[1])<<8 | int(p[2])<<16 }
	switch string(b[12:16]) {
	case "VP8X":
		return le24(b[24:27]) + 1, le24(b[27:30]) + 1, nil
	case "VP8L":
		if b[20] != 0x2f {
			return 0, 0, errors.New("bad VP8L header")
		}
		bits := binary.LittleEndian.Uint32(b[21:25])
		return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, nil
	case "VP8 ":
		if b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
			return 0, 0, errors.New("bad VP8 header")
		}
		return int(binary.LittleEndian.Uint16(b[26:28]) & 0x3fff), int(binary.LittleEndian.Uint16(b[28:30]) & 0x3fff), nil
	}
	return 0, 0, errors.New("unknown WebP chunk")
}

// CleanFileName makes a client's file name safe to show and to send as a document's name: the base
// name only, no invisible or control characters, at most 255 bytes. It is never used as a path.
func CleanFileName(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if InvisibleRune(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name))
	if name == "." || name == ".." {
		name = ""
	}
	if len(name) > maxQueueFileName {
		ext := filepath.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = strings.ToValidUTF8(name[:maxQueueFileName-len(ext)], "") + ext
	}
	if strings.TrimSuffix(name, filepath.Ext(name)) == "" {
		name = "file" + filepath.Ext(name)
	}
	return name
}

// QueueFiles lists live files: the actor's own, or everyone's for admin. Admins see other
// actors' files without their names.
func (b *Bot) QueueFiles(actor string, admin bool) ([]store.OutboxFile, error) {
	who := actor
	if admin {
		who = ""
	}
	files, err := b.Store.ListOutboxFiles(who)
	for i := range files {
		if files[i].Actor != actor {
			files[i].Name = ""
		}
	}
	return files, err
}

// QueueFileExpires is when a file no item has used yet is deleted, or zero once one has.
func QueueFileExpires(f store.OutboxFile) time.Time {
	if f.Items > 0 || !f.Deleted.IsZero() {
		return time.Time{}
	}
	return f.Created.Add(queueFileUnused)
}

// DeleteQueueFile deletes a file the actor uploaded (any actor's for admin) unless queued items
// still need it.
func (b *Bot) DeleteQueueFile(actor, id string, admin bool) (err error) {
	if !ValidQueueFileID(id) {
		return store.ErrNotFound
	}
	var f store.OutboxFile
	defer func() { b.Audit(actor, "queue_file_delete", id, fmt.Sprintf("%s, %d bytes", f.Kind, f.Size), err) }()
	owner := actor
	if admin {
		owner = ""
	}
	if f, err = b.Store.OutboxFile(id); err != nil {
		return err
	}
	if !f.Deleted.IsZero() || (owner != "" && f.Actor != owner) {
		f = store.OutboxFile{}
		return store.ErrNotFound
	}
	err = b.Store.DeleteOutboxFile(id, owner)
	if errors.Is(err, store.ErrOutboxFileInUse) {
		return FileInUseError{ID: id}
	}
	if err == nil {
		b.removeQueueFiles([]string{id})
	}
	return err
}

func (b *Bot) removeQueueFiles(ids []string) {
	for _, id := range ids {
		if err := os.Remove(b.queueFilePath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			b.Log.Error("queue file: remove", "id", id, "err", err)
		}
	}
}

// attachQueueFiles points the rows of reqs that name a file at it. Only the actor's own files can
// be used; anything else is "not found", so ids of other actors' files don't leak. A file deleted
// meanwhile is caught again inside Enqueue's transaction (the wame_outbox_file_live trigger).
func (b *Bot) attachQueueFiles(actor string, reqs []QueueRequest, where func(int) string, rows []store.OutboxItem) error {
	seen := map[string]store.OutboxFile{}
	for i, r := range reqs {
		if r.File == "" {
			continue
		}
		f, ok := seen[r.File]
		if !ok {
			var err error
			f, err = b.Store.OutboxFile(r.File)
			if errors.Is(err, store.ErrNotFound) || (err == nil && f.Actor != actor) {
				return fmt.Errorf("%s: no file %s: %w", where(i), r.File, store.ErrNotFound)
			}
			if err != nil {
				return err
			}
			seen[r.File] = f
		}
		rows[i].FileID, rows[i].FileKind, rows[i].FileSize = f.ID, f.Kind, f.Size
	}
	return nil
}

// queueFileErr turns Enqueue's trigger error for a deleted file into a 404.
func queueFileErr(err error) error {
	if store.IsOutboxFileGone(err) {
		return fmt.Errorf("a file of this request was deleted: %w", store.ErrNotFound)
	}
	return err
}

// queueFileUsage is the live files and bytes for QueueStats.
func (b *Bot) queueFileUsage() store.OutboxFileUsage {
	u, err := b.Store.OutboxFileUsage("")
	if err != nil {
		b.Log.Error("outbox: file usage", "err", err)
	}
	return u
}

// cachedUpload is what a WhatsApp upload returns that a message needs, stored per file so later
// items can reuse it. whatsmeow.UploadResponse hides the keys from JSON, hence a copy.
type cachedUpload struct {
	URL           string `json:"url"`
	DirectPath    string `json:"direct_path"`
	MediaKey      []byte `json:"media_key"`
	FileEncSHA256 []byte `json:"file_enc_sha256"`
	FileSHA256    []byte `json:"file_sha256"`
	FileLength    uint64 `json:"file_length"`
}

func (c cachedUpload) response() whatsmeow.UploadResponse {
	return whatsmeow.UploadResponse{URL: c.URL, DirectPath: c.DirectPath, MediaKey: c.MediaKey,
		FileEncSHA256: c.FileEncSHA256, FileSHA256: c.FileSHA256, FileLength: c.FileLength}
}

// queueMedia is a queued file ready to send: its row and an upload WhatsApp accepted.
type queueMedia struct {
	file   store.OutboxFile
	up     whatsmeow.UploadResponse
	reused bool
}

// readyMedia keeps the upload made for one item between the upload (before its slot) and the
// send, so a slot taken by someone else in between doesn't cost a second upload.
type readyMedia struct {
	item  int64
	media *queueMedia
	at    time.Time
}

func mediaType(kind string) whatsmeow.MediaType {
	if kind == store.FileImage {
		return whatsmeow.MediaImage
	}
	return whatsmeow.MediaDocument
}

// queueFile is the live file an item refers to, or errFileGone.
func (b *Bot) queueFile(it store.OutboxItem) (store.OutboxFile, error) {
	f, err := b.Store.OutboxFile(it.FileID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return f, errFileGone
	case err != nil:
		return f, err
	case !f.Deleted.IsZero() || f.Actor != it.Actor:
		return f, errFileGone
	}
	return f, nil
}

// queueMediaFor uploads an item's file to WhatsApp, or reuses an upload younger than
// queue_media_ttl. Only the outbox worker calls it, one item at a time.
func (b *Bot) queueMediaFor(ctx context.Context, it store.OutboxItem) (*queueMedia, error) {
	if m := b.readyFor(it.ID); m != nil {
		return m, nil
	}
	f, err := b.queueFile(it)
	if err != nil {
		return nil, err
	}
	ttl := b.currentSettings().QueueMediaTTL
	if ttl > 0 && len(f.Upload) > 0 && time.Since(f.UploadedAt) < ttl {
		var c cachedUpload
		if json.Unmarshal(f.Upload, &c) == nil && c.DirectPath != "" && len(c.MediaKey) > 0 {
			return b.setReady(it.ID, &queueMedia{file: f, up: c.response(), reused: true}), nil
		}
	}
	data, err := os.ReadFile(b.queueFilePath(f.ID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errFileGone
	}
	if err != nil {
		b.Log.Error("outbox: read queued file", "id", f.ID, "err", err)
		return nil, errFileUnreadable
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != f.SHA256 {
		return nil, errFileGone
	}
	uctx, cancel := context.WithTimeout(ctx, queueUploadTimeout)
	defer cancel()
	up, err := b.uploadMedia(uctx, data, mediaType(f.Kind))
	if err != nil {
		return nil, uploadError(err)
	}
	if ttl > 0 {
		blob, _ := json.Marshal(cachedUpload{URL: up.URL, DirectPath: up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: up.FileLength})
		if err := b.Store.SetOutboxUpload(f.ID, blob, time.Now()); err != nil {
			b.Log.Error("outbox: cache upload", "err", err)
		}
	}
	return b.setReady(it.ID, &queueMedia{file: f, up: up}), nil
}

// readyFor is the upload kept for this item, or nil.
func (b *Bot) readyFor(item int64) *queueMedia {
	b.outbox.mu.Lock()
	defer b.outbox.mu.Unlock()
	if r := b.outbox.ready; r != nil && r.item == item && time.Since(r.at) < queueReadyFor {
		return r.media
	}
	return nil
}

func (b *Bot) setReady(item int64, m *queueMedia) *queueMedia {
	b.outbox.mu.Lock()
	b.outbox.ready = &readyMedia{item: item, media: m, at: time.Now()}
	b.outbox.mu.Unlock()
	return m
}

func (b *Bot) clearReady() {
	b.outbox.mu.Lock()
	b.outbox.ready = nil
	b.outbox.mu.Unlock()
}

var urlRest = regexp.MustCompile(`(https?://[^/\s"?]+)[^\s"]*`)

// uploadError cuts URLs in an upload error down to their host: the upload URL carries WhatsApp's
// media auth token in its query, and these errors reach the audit log, API answers, queue items,
// events and the webhook.
// It still unwraps to the cause, so a canceled context is still recognized.
func uploadError(err error) error {
	return scrubbedError{"upload: " + urlRest.ReplaceAllString(err.Error(), "$1"), err}
}

type scrubbedError struct {
	msg string
	err error
}

func (e scrubbedError) Error() string { return e.msg }
func (e scrubbedError) Unwrap() error { return e.err }

// queuedFile gets a file item's upload before the worker takes its slot, so a slow upload
// doesn't hold a slot that interactive sends need. It uploads only when the slot is (nearly)
// free, and otherwise returns how long to wait. Text items pass through.
func (b *Bot) queuedFile(ctx context.Context, it store.OutboxItem, tok *store.Token, chat string) (*queueMedia, time.Duration, error) {
	if it.FileID == "" {
		return nil, 0, nil
	}
	if m := b.readyFor(it.ID); m != nil {
		return m, 0, nil
	}
	if _, wait := b.outboxPeek(tok, chat); wait > outboxSlotWithin {
		return nil, wait, nil
	}
	m, err := b.queueMediaFor(ctx, it)
	if err == nil && !m.reused {
		// An upload can take minutes: pause, quiet hours and the token are checked again by the
		// next step, which finds this upload kept for the item.
		return nil, redoStep, nil
	}
	return m, 0, err
}

// redoStep is queuedFile's answer after a fresh upload: end this step, send on the next one.
const redoStep = -1

// clientOnline reports whether WhatsApp is reachable; tests swap it.
var clientOnline = func(b *Bot) bool { return b.Client.IsLoggedIn() && b.Client.IsConnected() }

// queuedFileFailed records a failed upload as an attempt: the item goes back to the queue with the
// usual backoff (or fails after the last attempt, or at once when the file is gone), and the
// error counts toward the whole-queue cooldown.
func (b *Bot) queuedFileFailed(ctx context.Context, it store.OutboxItem, err error) {
	if ctx.Err() != nil {
		return
	}
	claimed, cerr := b.Store.ClaimOutbox(it.ID, string(b.Client.GenerateMessageID()))
	if cerr != nil {
		if !errors.Is(cerr, store.ErrNotFound) {
			b.Log.Error("outbox: claim", "id", it.ID, "err", cerr)
		}
		return
	}
	r := store.OutboxResult{Status: store.OutboxQueued, Error: err.Error()}
	switch {
	case errors.Is(err, errFileGone):
		r.Status = store.OutboxFailed
	case !clientOnline(b):
		r.Refund = true // the connection dropped, like a send attempt while offline
	case errors.Is(err, errFileUnreadable):
		// A local problem, not WhatsApp pushing back: no whole-queue cooldown.
		r.Due = time.Now().Add(outboxBackoff(claimed.Attempts))
		if claimed.Attempts >= outboxAttempts {
			r.Status = store.OutboxFailed
		}
	default:
		b.outboxResult(false, r.Error)
		if claimed.Attempts >= outboxAttempts {
			r.Status = store.OutboxFailed
		} else {
			r.Due = time.Now().Add(outboxBackoff(claimed.Attempts))
		}
	}
	b.finishQueued(claimed, r)
}

// sendQueuedItem sends a claimed item: text through SendText, a file through sendQueuedFile.
func (b *Bot) sendQueuedItem(ctx context.Context, it store.OutboxItem, media *queueMedia) (store.Message, error) {
	opt := SendOptions{ReplyTo: it.ReplyTo, queued: it.ID, messageID: types.MessageID(it.MessageID)}
	if it.FileID == "" {
		return b.SendText(ctx, it.Actor, it.To, it.Text, opt)
	}
	defer b.clearReady()
	return b.audited(it.Actor, "send_file", it.To, fmt.Sprintf("%s, %d bytes, queue #%d", it.FileKind, it.FileSize, it.ID), func() (store.Message, error) {
		jid, quote, err := b.prepare(ctx, it.To, opt)
		if err != nil {
			return store.Message{}, err
		}
		if err := b.optedOut(jid, it.To); err != nil {
			return store.Message{}, err
		}
		if media == nil {
			if media, err = b.queueMediaFor(ctx, it); err != nil {
				return store.Message{}, err
			}
		}
		if f, err := b.queueFile(it); err != nil {
			return store.Message{}, err // deleted (or logged out) since the upload
		} else if f.SHA256 != media.file.SHA256 {
			return store.Message{}, errFileGone
		}
		f := media.file
		msg, rec := fileMessage(media.up, f.Kind == store.FileImage, f.Mime, f.Name, it.Text, quote, uint64(f.Size))
		rec.ReplyTo = it.ReplyTo
		m, err := b.deliver(ctx, jid, msg, rec, opt)
		b.dropReusedUpload(media, err)
		return m, err
	})
}

// dropReusedUpload forgets a file's cached upload after a send that used it failed: the CDN
// object may have expired, so the next attempt uploads again.
func (b *Bot) dropReusedUpload(m *queueMedia, err error) {
	if err == nil || !m.reused {
		return
	}
	if cerr := b.Store.SetOutboxUpload(m.file.ID, nil, time.Time{}); cerr != nil {
		b.Log.Error("outbox: drop cached upload", "err", cerr)
	}
}

// fileMessage builds the message for an uploaded file: a photo, or a document with its name.
func fileMessage(up whatsmeow.UploadResponse, photo bool, mime, fileName, caption string, quote *waE2E.ContextInfo, size uint64) (*waE2E.Message, store.Message) {
	rec := store.Message{Type: "document", Text: caption, Media: &store.Media{Mimetype: mime, FileName: fileName, Size: size}}
	if caption == "" {
		rec.Text = fileName
	}
	if photo {
		rec.Type, rec.Text, rec.Media.FileName = "image", caption, ""
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Caption: new(caption), Mimetype: new(mime), ContextInfo: quote,
			URL: new(up.URL), DirectPath: new(up.DirectPath), MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: new(up.FileLength),
		}}, rec
	}
	return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		Caption: new(caption), Mimetype: new(mime), ContextInfo: quote,
		FileName: new(fileName), Title: new(fileName),
		URL: new(up.URL), DirectPath: new(up.DirectPath), MediaKey: up.MediaKey,
		FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: new(up.FileLength),
	}}, rec
}

// pruneQueueFiles deletes files nothing open needs (an hour after their last item finished, or a
// day after upload when no item used them), files of tokens that were revoked or expired, rows
// no item refers to any more, and files on disk without a row.
func (b *Bot) pruneQueueFiles() {
	now := time.Now()
	ids, err := b.Store.ExpireOutboxFiles(now.Add(-queueFileAfterDone), now.Add(-queueFileUnused))
	if err != nil {
		b.Log.Error("outbox: expire files", "err", err)
	}
	tokens, err := b.Store.OutboxFileTokens()
	if err != nil {
		b.Log.Error("outbox: file tokens", "err", err)
	}
	seen := map[string]*store.Token{}
	for _, id := range tokens {
		if _, err := b.queueToken(id, seen); err == nil || errors.Is(err, errTokenLookup) {
			continue
		}
		gone, err := b.Store.DeleteOutboxFilesOf(id)
		if err != nil {
			b.Log.Error("outbox: delete files of a token", "err", err)
		}
		ids = append(ids, gone...)
	}
	b.removeQueueFiles(ids)
	if len(ids) > 0 {
		b.Log.Info("outbox: deleted files", "count", len(ids))
	}
	if _, err := b.Store.PruneOutboxFiles(); err != nil {
		b.Log.Error("outbox: prune file rows", "err", err)
	}
	b.sweepQueueFiles(now.Add(-queueFileSweepAge))
}

// sweepQueueFiles removes files in outbox/ that have no live row and weren't written since
// before: left by a crash between writing a file and storing its row, or by a failed delete.
func (b *Bot) sweepQueueFiles(before time.Time) {
	entries, err := os.ReadDir(b.queueDir())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			b.Log.Error("outbox: read file directory", "err", err)
		}
		return
	}
	live, err := b.Store.LiveOutboxFiles()
	if err != nil {
		b.Log.Error("outbox: live files", "err", err)
		return
	}
	keep := map[string]bool{}
	for _, id := range live {
		keep[id] = true
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !ValidQueueFileID(e.Name()) || keep[e.Name()] {
			continue
		}
		if info, err := e.Info(); err != nil || info.ModTime().After(before) {
			continue
		}
		b.removeQueueFiles([]string{e.Name()})
	}
}

// wipeQueueFiles deletes every queued file (logout), audited when there was anything to delete.
// Only names wame chose are removed, and the directory only when that leaves it empty. Items that
// still refer to a file fail when their turn comes.
func (b *Bot) wipeQueueFiles(actor string) {
	n, err := b.Store.WipeOutboxFiles()
	if err == nil {
		b.clearReady()
		err = b.removeQueueDir()
	}
	if n > 0 || err != nil {
		b.Audit(actor, "queue_files_wipe", "", fmt.Sprintf("%d files", n), err)
	}
}

// removeQueueDir deletes the files wame stored in the queue directory, then the directory if it's empty.
func (b *Bot) removeQueueDir() error {
	dir := b.queueDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Type().IsRegular() && ValidQueueFileID(e.Name()) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		b.Log.Warn("outbox: queue file directory kept", "err", err)
	}
	return nil
}
