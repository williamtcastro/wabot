package bot

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
)

// sciNumber is how spreadsheets show long numbers they treat as numbers: 5.511999E+12; decNumber
// is how exports (pandas, a column of floats) write them: 5511999999999.0.
var (
	sciNumber = regexp.MustCompile(`^\+?\d+(?:[.,]\d+)?[eE]\+?\d+$`)
	decNumber = regexp.MustCompile(`^\+?\d+[.,]\d+$`)
)

// ParseSendAt reads a send_at time, which must be RFC 3339.
func ParseSendAt(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, fmt.Errorf("%.40q isn't an RFC 3339 time like 2026-10-13T09:00:00-03:00", s)
	}
	return t, nil
}

// QueueColumns are the columns a queue CSV may have, in QueueRecord's order.
var QueueColumns = []string{"to", "text", "send_at", "reply_to"}

// ReadQueueCSV reads a header row naming the columns (to required; text, send_at and reply_to
// optional; any order, any case) and one message per record. A leading BOM, CRLF line ends, quoted
// fields, a ';' separator (spreadsheets in locales with a decimal comma), short records and
// unnamed columns (a spreadsheet's empty trailing ones) are handled; fields are trimmed and blank
// records skipped. With limit > 0, more records than that is an error. Errors name the line.
func ReadQueueCSV(r io.Reader, limit int) ([]QueueRequest, error) {
	br := bufio.NewReader(r)
	if bom, err := br.Peek(3); err == nil && string(bom) == "\xef\xbb\xbf" {
		_, _ = br.Discard(3)
	}
	cr := csv.NewReader(br)
	cr.TrimLeadingSpace = true
	cr.ReuseRecord = true
	cr.FieldsPerRecord = -1
	head, _ := br.Peek(br.Size())
	if i := bytes.IndexByte(head, '\n'); i >= 0 {
		head = head[:i]
	}
	if !bytes.ContainsRune(head, ',') && bytes.ContainsRune(head, ';') {
		cr.Comma = ';'
	}
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, BadInput("the CSV is empty: the first line must name the columns, like to,text")
	}
	if err != nil {
		return nil, csvError(err)
	}
	hline, _ := cr.FieldPos(0)
	col, named := map[string]int{}, map[int]bool{}
	var unknown []string
	for i, h := range header {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if _, dup := col[h]; dup {
			return nil, BadInput("line %d: column %q appears twice", hline, h)
		}
		if !slices.Contains(QueueColumns, h) {
			if len(unknown) <= 5 {
				unknown = append(unknown, fmt.Sprintf("%.40q", h))
			}
			continue
		}
		col[h], named[i] = i, true
	}
	if len(unknown) > 0 {
		if len(unknown) > 5 {
			unknown[5] = "…"
		}
		hint := ""
		if slices.ContainsFunc(unknown, func(u string) bool { return strings.Contains(u, ";") }) {
			hint = "; export the file as comma-separated CSV"
		}
		return nil, BadInput("line %d: unknown columns %s (use %s)%s", hline, strings.Join(unknown, ", "), strings.Join(QueueColumns, ", "), hint)
	}
	if _, ok := col["to"]; !ok {
		return nil, BadInput(`line %d: the "to" column is required`, hline)
	}
	field := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var rows []QueueRequest
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, csvError(err)
		}
		line, _ := cr.FieldPos(0)
		if !slices.ContainsFunc(rec, func(f string) bool { return strings.TrimSpace(f) != "" }) {
			continue
		}
		for i, f := range rec {
			if !named[i] && strings.TrimSpace(f) != "" {
				return nil, BadInput("line %d: field %d is outside the named columns (quote text that has a %q in it)", line, i+1, cr.Comma)
			}
		}
		if limit > 0 && len(rows) == limit {
			return nil, BadInput("line %d: at most %d messages per request", line, limit)
		}
		row := QueueRequest{Line: line, To: field(rec, "to"), Text: field(rec, "text"), ReplyTo: field(rec, "reply_to")}
		switch {
		case sciNumber.MatchString(row.To):
			return nil, BadInput("line %d: %.40q is a number in scientific notation, which has lost digits; format the column as text in the spreadsheet", line, row.To)
		case decNumber.MatchString(row.To):
			return nil, BadInput("line %d: %.40q is a number with decimals, not a phone number; format the column as text in the spreadsheet", line, row.To)
		}
		if s := field(rec, "send_at"); s != "" {
			if row.SendAt, err = ParseSendAt(s); err != nil {
				return nil, BadInput("line %d: send_at %v", line, err)
			}
		}
		rows = append(rows, row)
	}
}

// csvError turns a parse error into bad input naming the line; read errors (like a body over its
// limit) pass through.
func csvError(err error) error {
	if pe, ok := errors.AsType[*csv.ParseError](err); ok {
		return BadInput("line %d: %v", pe.Line, pe.Err)
	}
	return err
}

// QueueRecord is r as a record under the QueueColumns header, in the form ReadQueueCSV reads.
func QueueRecord(r QueueRequest) []string {
	at := ""
	if !r.SendAt.IsZero() {
		at = r.SendAt.Format(time.RFC3339Nano)
	}
	return []string{r.To, r.Text, at, r.ReplyTo}
}
