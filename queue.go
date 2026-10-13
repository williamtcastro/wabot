package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/williamtcastro/wame/internal/admin"
	"github.com/williamtcastro/wame/internal/bot"
)

const (
	maxImportFile = 64 << 20
	maxChunkBytes = 15 << 20 // under the server's 16 MB body limit
)

// chunkMax is the rows per request: the server's limit (tests lower it).
var chunkMax = bot.MaxQueueBatch

// queueCommand is `wame queue import|status`: it talks to a running server over its HTTP API, never
// to the local database, since the server owns the WhatsApp session.
func queueCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: wame queue import [flags] FILE.csv | wame queue status [-batch NAME]")
	}
	switch args[0] {
	case "import":
		return queueImport(ctx, args[1:], out)
	case "status":
		return queueStatus(ctx, args[1:], out)
	}
	return fmt.Errorf("unknown queue command %q (use import or status)", args[0])
}

// importChunk is one POST /queue: up to chunkMax rows and maxChunkBytes of CSV.
type importChunk struct {
	body        []byte
	rows        int
	first, last int
	lines       map[int]int // line in body → line in the file
}

func queueImport(ctx context.Context, args []string, out io.Writer) error {
	const use = "usage: wame queue import [-batch NAME] [-text DEFAULT] [-send-at TIME] [-key KEY] [-dry-run] FILE.csv"
	file := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		file, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("queue import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var sf serverFlags
	sf.register(fs)
	batch := fs.String("batch", "", "")
	text := fs.String("text", "", "")
	sendAt := fs.String("send-at", "", "")
	key := fs.String("key", "", "")
	dryRun := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, use)
	}
	if file == "" && fs.NArg() == 1 {
		file = fs.Arg(0)
	} else if fs.NArg() > 0 {
		return errors.New(use)
	}
	if file == "" {
		return errors.New(use)
	}
	var at time.Time
	if *sendAt != "" {
		var err error
		if at, err = time.Parse(time.RFC3339, *sendAt); err != nil {
			return errors.New("-send-at must be an RFC 3339 time like 2026-10-13T09:00:00-03:00")
		}
	}

	data, err := readImportFile(file)
	if err != nil {
		return err
	}
	rows, err := bot.ReadQueueCSV(bytes.NewReader(data), 0)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("%s: no messages, only the header", file)
	}
	for i := range rows {
		rows[i].Text, rows[i].SendAt = cmp.Or(rows[i].Text, *text), cmp.Or(rows[i].SendAt, at)
		if err := bot.CheckQueueRequest(rows[i]); err != nil {
			return fmt.Errorf("%s: %s: %w", file, rows[i].Where(i), err)
		}
	}
	sum := sha256.Sum256(data)
	base := cmp.Or(*key, "csv-"+hex.EncodeToString(sum[:16]))
	if len(base) > bot.MaxKeyLength-8 {
		return fmt.Errorf("-key is longer than %d characters", bot.MaxKeyLength-8)
	}
	keySum := sha256.Sum256([]byte(base))
	*batch = cmp.Or(*batch, "csv-"+hex.EncodeToString(keySum[:6]))
	if err := bot.CheckBatch(*batch); err != nil {
		return err
	}
	chunks, err := chunkRows(rows)
	if err != nil {
		return err
	}

	if *dryRun {
		fmt.Fprintf(out, "%s: %d messages to %d recipients, batch %s, in %d request(s):\n", file, len(rows), recipients(rows), *batch, len(chunks))
		for i, c := range chunks {
			fmt.Fprintf(out, "  %d/%d: lines %d-%d, %d messages, %s, key %s\n", i+1, len(chunks), c.first, c.last, c.rows, byteSize(int64(len(c.body))), chunkKey(base, i))
		}
		fmt.Fprintln(out, "dry run: nothing sent")
		return nil
	}

	target, _, err := sf.resolve()
	if err != nil {
		return err
	}
	c, err := target.client()
	if err != nil {
		return err
	}
	queued := 0
	for i, ch := range chunks {
		res, err := c.QueueCSV(ctx, ch.body, *batch, chunkKey(base, i))
		if err != nil {
			fmt.Fprintf(out, "%d/%d: lines %d-%d: failed: %v\n", i+1, len(chunks), ch.first, ch.last, ch.fileError(err))
			if apiErr, ok := errors.AsType[*admin.APIError](err); ok && apiErr.Code == http.StatusConflict {
				return fmt.Errorf("stopped after %d of %d messages: these rows differ from what was queued under key %s before; for a new import, pass another -key", queued, len(rows), chunkKey(base, i))
			}
			return fmt.Errorf("stopped after %d of %d messages; run the same command again to retry: what was already queued isn't queued twice", queued, len(rows))
		}
		queued += res.Queued
		fmt.Fprintf(out, "%d/%d: lines %d-%d: queued %d\n", i+1, len(chunks), ch.first, ch.last, res.Queued)
	}
	fmt.Fprintf(out, "queued %d messages, batch %s (follow it with: wame queue status -batch %s)\n", queued, *batch, *batch)
	return nil
}

func readImportFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxImportFile+1))
	if err == nil && len(data) > maxImportFile {
		err = fmt.Errorf("%s is larger than %s", path, byteSize(maxImportFile))
	}
	return data, err
}

// chunkRows splits rows into requests the server accepts, always at the same places for the same
// input, so each chunk keeps its idempotency key when the import is run again.
func chunkRows(rows []bot.QueueRequest) ([]importChunk, error) {
	var chunks []importChunk
	var buf, rec bytes.Buffer
	var cur importChunk
	lines := 0
	flush := func() {
		if cur.rows > 0 {
			cur.body = bytes.Clone(buf.Bytes())
			chunks = append(chunks, cur)
		}
		buf.Reset()
		cur = importChunk{}
	}
	for _, r := range rows {
		rec.Reset()
		if err := writeRecord(&rec, bot.QueueRecord(r)); err != nil {
			return nil, err
		}
		if cur.rows == chunkMax || (cur.rows > 0 && buf.Len()+rec.Len() > maxChunkBytes) {
			flush()
		}
		if cur.rows == 0 {
			_ = writeRecord(&buf, bot.QueueColumns)
			cur.first, cur.lines, lines = r.Line, map[int]int{}, 1
		}
		cur.lines[lines+1] = r.Line
		lines += bytes.Count(rec.Bytes(), []byte("\n"))
		buf.Write(rec.Bytes())
		cur.rows++
		cur.last = r.Line
	}
	flush()
	return chunks, nil
}

func writeRecord(w io.Writer, rec []string) error {
	cw := csv.NewWriter(w)
	_ = cw.Write(rec)
	cw.Flush()
	return cw.Error()
}

var chunkLine = regexp.MustCompile(`^line (\d+): `)

// fileError rewrites a server error's "line N" (counted in the chunk it was sent) to the file's line.
func (c importChunk) fileError(err error) error {
	apiErr, ok := errors.AsType[*admin.APIError](err)
	if !ok {
		return err
	}
	m := chunkLine.FindStringSubmatch(apiErr.Msg)
	if m == nil {
		return err
	}
	n, _ := strconv.Atoi(m[1])
	if line, ok := c.lines[n]; ok {
		return &admin.APIError{Code: apiErr.Code, Msg: fmt.Sprintf("line %d: %s", line, apiErr.Msg[len(m[0]):])}
	}
	return err
}

func chunkKey(base string, i int) string { return fmt.Sprintf("%s.%d", base, i+1) }

func recipients(rows []bot.QueueRequest) int {
	seen := map[string]bool{}
	for _, r := range rows {
		jid, _ := bot.ParseRecipient(r.To)
		seen[jid.String()] = true
	}
	return len(seen)
}

func queueStatus(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("queue status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var sf serverFlags
	sf.register(fs)
	batch := fs.String("batch", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return errors.New("usage: wame queue status [-batch NAME] [-server URL] [-profile NAME]")
	}
	target, _, err := sf.resolve()
	if err != nil {
		return err
	}
	c, err := target.client()
	if err != nil {
		return err
	}
	n, err := c.QueueCounts(ctx, *batch)
	if err != nil {
		return err
	}
	if *batch != "" {
		fmt.Fprintf(out, "batch %s: ", *batch)
	}
	fmt.Fprintf(out, "queued %d, sending %d, sent %d (delivered %d, read %d), failed %d, canceled %d\n",
		n.Queued, n.Sending, n.Sent, n.Delivered, n.Read, n.Failed, n.Canceled)
	if !n.NextDue.IsZero() {
		fmt.Fprintf(out, "next due %s\n", n.NextDue.Local().Format("2006-01-02 15:04"))
	}
	return nil
}
