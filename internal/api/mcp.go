package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/williamtcastro/wame/internal/auth"
	"github.com/williamtcastro/wame/internal/bot"
	"github.com/williamtcastro/wame/internal/mcpserver"
)

// mcpHandler serves MCP over streamable HTTP at /mcp. With tokens configured, each request's bearer
// token becomes the tool caller's principal (scopes and chat allowlist apply per call); the SDK also
// pins every MCP session to the token that opened it. In token-less mode it's loopback-only, as the API.
func mcpHandler(b *bot.Bot, authn *auth.Authenticator, fails *failureLimiter, version string, log *slog.Logger) http.Handler {
	owner := auth.Owner()
	srv := mcpserver.New(b, version, &owner)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Logger:         log,
		SessionTimeout: 30 * time.Minute,
	})
	verify := func(_ context.Context, token string, r *http.Request) (*sdkauth.TokenInfo, error) {
		p, err := authn.Authenticate(token)
		if err != nil {
			fails.record(clientIP(r))
			return nil, fmt.Errorf("%w: %v", sdkauth.ErrInvalidToken, err)
		}
		b.RecordRequest(p.Name, clientIP(r))
		info := &sdkauth.TokenInfo{UserID: "owner", Expiration: time.Now().Add(time.Hour), Extra: map[string]any{mcpserver.PrincipalKey: p}}
		if p.TokenID != "" {
			info.UserID = p.TokenID
		}
		for _, s := range p.Scopes {
			info.Scopes = append(info.Scopes, string(s))
		}
		return info, nil
	}
	withToken := sdkauth.RequireBearerToken(verify, nil)(h)
	local := localOnly(h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fails.reject(w, clientIP(r)) {
			return
		}
		if authn.Required() {
			withToken.ServeHTTP(w, r)
			return
		}
		local.ServeHTTP(w, r)
	})
}
