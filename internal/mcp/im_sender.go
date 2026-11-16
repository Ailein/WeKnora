package mcp

import (
	"context"
	"net/url"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// imSenderForwardHeader is the opt-in switch for telling an MCP server which
// IM user is behind a tool call. A service that lists this header among its
// configured request headers (自定义请求头) with a value other than "", "0" or
// "false" receives X-IM-Platform, X-IM-Channel-Id, X-IM-User-Id and (when the
// platform gives one) X-IM-Message-Id on every request made while answering an
// IM message. A service that does not list it is told nothing: an IM user id is
// often a phone number, and it should not reach a third-party server that did
// not ask for it.
//
// The server then decides on facts the model cannot change (who wrote the
// message, and which message the run is answering), instead of on what the
// model says about the sender. The message id lets a server require that a
// second step was asked for in a later message than the first: a step the model
// takes by itself in the same run carries the same id.
const imSenderForwardHeader = "X-Forward-IM-Sender"

// wantsIMSender reports whether the service's configured headers opt in.
func wantsIMSender(headers map[string]string) bool {
	for key, value := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), imSenderForwardHeader) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", "0", "false":
			return false
		}
		return true
	}
	return false
}

// imSenderHeaders names the IM user a request is made for, or returns nil when
// the caller is not an IM user (the web console, an API key, the connection
// handshake, a directory refresh).
//
// withIMIdentity (internal/im/service.go) builds the principal id as
// "<tenantID>:<channelID>:<platform>:<userID>". The user id comes last because
// it is the one part that may itself contain a colon. Values are
// percent-encoded: an id with a character HTTP does not allow in a header
// would otherwise fail the whole call.
func imSenderHeaders(ctx context.Context) map[string]string {
	principal, ok := types.PrincipalFromContext(ctx)
	if !ok || principal.Type != types.PrincipalIMUser {
		return nil
	}
	parts := strings.SplitN(principal.ID, ":", 4)
	if len(parts) != 4 || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return nil
	}
	headers := map[string]string{
		"X-IM-Channel-Id": url.PathEscape(parts[1]),
		"X-IM-Platform":   url.PathEscape(parts[2]),
		"X-IM-User-Id":    url.PathEscape(parts[3]),
	}
	if messageID := types.IMMessageIDFromContext(ctx); messageID != "" {
		headers["X-IM-Message-Id"] = url.PathEscape(messageID)
	}
	return headers
}
