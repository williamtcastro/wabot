package bot

import (
	"context"
	"io"
	"net/http"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"github.com/williamtcastro/wabot/internal/store"
)

const maxMediaDownload = 100 << 20

// DownloadMedia fetches and decrypts the media of a stored message.
func (b *Bot) DownloadMedia(ctx context.Context, seq int64) ([]byte, store.Message, error) {
	m, err := b.Store.GetBySeq(seq)
	if err != nil {
		return nil, m, err
	}
	if m.Media == nil || len(m.Raw) == 0 {
		return nil, m, store.ErrNotFound
	}
	if m.Media.Size > maxMediaDownload {
		return nil, m, BadInput("file is %d MB, over the %d MB download limit", m.Media.Size>>20, maxMediaDownload>>20)
	}
	var msg waE2E.Message
	if err := proto.Unmarshal(m.Raw, &msg); err != nil {
		return nil, m, err
	}
	_, _, _, media := classify(&msg)
	if media == nil {
		return nil, m, store.ErrNotFound
	}
	data, err := b.Client.Download(ctx, media)
	return data, m, err
}

// limitedTransport refuses media downloads bigger than maxMediaDownload, whatever size the sender declared.
type limitedTransport struct{ http.RoundTripper }

func (t limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(req)
	if err == nil && req.Method == http.MethodGet {
		resp.Body = &cappedBody{rc: resp.Body, left: maxMediaDownload}
	}
	return resp, err
}

// cappedBody fails with a plain (non-network) error past the limit, which whatsmeow treats as final.
type cappedBody struct {
	rc   io.ReadCloser
	left int64
}

func (c *cappedBody) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, BadInput("file is over the %d MB download limit", maxMediaDownload>>20)
	}
	n, err := c.rc.Read(p[:min(int64(len(p)), c.left)])
	c.left -= int64(n)
	if c.left <= 0 && err == nil {
		if extra, _ := c.rc.Read(make([]byte, 1)); extra > 0 {
			return n, BadInput("file is over the %d MB download limit", maxMediaDownload>>20)
		}
		err = io.EOF
	}
	return n, err
}

func (c *cappedBody) Close() error { return c.rc.Close() }
