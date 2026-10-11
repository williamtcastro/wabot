package store

import (
	"errors"
	"fmt"
	"mime"
	"strings"
	"time"
)

type Message struct {
	Seq       int64     `json:"seq"`
	ID        string    `json:"id"`
	Chat      string    `json:"chat"`
	Sender    string    `json:"sender"`
	PushName  string    `json:"push_name"`
	FromMe    bool      `json:"from_me"`
	IsGroup   bool      `json:"is_group"`
	Type      string    `json:"type"`
	Text      string    `json:"text"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	Media     *Media    `json:"media,omitempty"`
	Edited    bool      `json:"edited,omitempty"`
	Deleted   bool      `json:"deleted,omitempty"`
	Timestamp time.Time `json:"timestamp"`

	// Raw is the original protobuf; RawChat and RawSender are the JIDs as WhatsApp sent them.
	Raw       []byte `json:"-"`
	RawChat   string `json:"-"`
	RawSender string `json:"-"`
}

type Media struct {
	Mimetype string `json:"mimetype"`
	FileName string `json:"file_name,omitempty"`
	Size     uint64 `json:"size"`
	Path     string `json:"path"`
}

// MessageUpdate is published when a stored message is edited or deleted.
type MessageUpdate struct{ Message }

type Chat struct {
	JID        string    `json:"jid"`
	Name       string    `json:"name"`
	IsGroup    bool      `json:"is_group"`
	LastText   string    `json:"last_text"`
	LastType   string    `json:"last_type"`
	LastFromMe bool      `json:"last_from_me"`
	LastAt     time.Time `json:"last_at"`
	Unread     int       `json:"unread"`
}

// ChatsChanged is published after a history sync bulk-imports messages.
type ChatsChanged struct{}

var ErrNotFound = errors.New("message not found")

// MediaFileName prefers our table because mime.ExtensionsByType sorts alphabetically (.jfif for JPEG).
// m.Media must be set.
func (m Message) MediaFileName() string {
	if m.Media.FileName != "" {
		return m.Media.FileName
	}
	ext := extByType[strings.SplitN(m.Media.Mimetype, ";", 2)[0]]
	if ext == "" {
		if exts, _ := mime.ExtensionsByType(m.Media.Mimetype); len(exts) > 0 {
			ext = exts[0]
		}
	}
	return fmt.Sprintf("wabot-%s-%d%s", m.Type, m.Seq, ext)
}

var extByType = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "video/mp4": ".mp4",
	"audio/ogg": ".ogg", "audio/mpeg": ".mp3", "application/pdf": ".pdf",
}

func mediaPath(seq int64) string { return fmt.Sprintf("/media/%d", seq) }
