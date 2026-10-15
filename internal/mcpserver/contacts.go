package mcpserver

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
)

type contactProfileIn struct {
	Chat string `json:"chat" jsonschema:"phone number with country code or a person's JID"`
}

type groupOut struct {
	Chat string `json:"chat"`
	Name string `json:"name"`
}

type contactProfileOut struct {
	Chat          string    `json:"chat"`
	Phone         string    `json:"phone,omitempty"`
	LID           string    `json:"lid,omitempty"`
	NameSource    string    `json:"name_source"`
	OnWhatsApp    *bool     `json:"on_whatsapp"`
	VerifiedLevel string    `json:"verified_level,omitempty"`
	FirstAt       time.Time `json:"first_at,omitzero"`
	LastAt        time.Time `json:"last_at,omitzero"`
	Sent          int       `json:"sent"`
	Received      int       `json:"received"`
	Unread        int       `json:"unread"`
	Untrusted     struct {
		Name           string     `json:"name"`
		OwnName        string     `json:"own_name,omitempty"`
		BookName       string     `json:"book_name,omitempty"`
		BusinessName   string     `json:"business_name,omitempty"`
		PushName       string     `json:"push_name,omitempty"`
		Tags           []string   `json:"tags"`
		About          string     `json:"about,omitempty"`
		VerifiedName   string     `json:"verified_name,omitempty"`
		GroupsInCommon []groupOut `json:"groups_in_common"`
	} `json:"untrusted"`
	Note string `json:"note"`
}

func (s *server) addContactTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "contact_profile",
		Description: "One person's profile from what wame already knows (never asks WhatsApp): names, tags, about, first and last message, counts and groups in common.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, s.contactProfile)
}

// personChat checks a chat argument without contacting WhatsApp for anyone: unlike chat(), an
// unrestricted caller's number is parsed locally too, since a profile never needs the canonical JID.
func (s *server) personChat(ctx context.Context, p auth.Principal, chat string) (string, error) {
	if strings.TrimSpace(chat) == "" {
		return "", errors.New("chat is required")
	}
	if p.Restricted() {
		return s.allowedChat(ctx, p, chat)
	}
	return chat, nil
}

func (s *server) contactProfile(ctx context.Context, req *mcp.CallToolRequest, in contactProfileIn) (*mcp.CallToolResult, contactProfileOut, error) {
	p, err := s.require(req, auth.Read)
	if err != nil {
		return nil, contactProfileOut{}, err
	}
	chat, err := s.personChat(ctx, p, in.Chat)
	if err != nil {
		return nil, contactProfileOut{}, err
	}
	pr, err := s.b.Profile(ctx, chat, bot.ProfileOptions{Details: true})
	if err != nil {
		return nil, contactProfileOut{}, err
	}
	out := contactProfileOut{Chat: pr.JID, Phone: pr.Phone, LID: cleanID(pr.LID), NameSource: pr.Names.Source,
		OnWhatsApp: pr.WhatsApp.OnWhatsApp, VerifiedLevel: cleanID(pr.WhatsApp.VerifiedLevel), Note: untrustedNote}
	if a := pr.Activity; a != nil {
		out.FirstAt, out.LastAt, out.Sent, out.Received, out.Unread = a.FirstAt, a.LastAt, a.Sent, a.Received, a.Unread
	}
	u := &out.Untrusted
	u.Name, u.OwnName, u.BookName = clean(pr.Names.Display), clean(pr.Names.Own), clean(pr.Names.Book)
	u.BusinessName, u.PushName = clean(pr.Names.Business), clean(pr.Names.Push)
	u.About, u.VerifiedName = clean(pr.WhatsApp.About), clean(pr.WhatsApp.VerifiedName)
	u.Tags = make([]string, len(pr.Own.Tags))
	for i, t := range pr.Own.Tags {
		u.Tags[i] = clean(t)
	}
	u.GroupsInCommon = []groupOut{}
	if a := pr.Activity; a != nil {
		for _, g := range a.GroupsInCommon {
			if p.Restricted() && !s.b.ChatAllowed(ctx, p.Chats, g.JID) {
				continue
			}
			u.GroupsInCommon = append(u.GroupsInCommon, groupOut{Chat: g.JID, Name: clean(g.Name)})
		}
	}
	return nil, out, nil
}
