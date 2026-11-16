package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
	sdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
)

func TestWantsIMSender(t *testing.T) {
	require.False(t, wantsIMSender(nil))
	require.False(t, wantsIMSender(map[string]string{"X-API-Key": "k"}))
	require.False(t, wantsIMSender(map[string]string{"X-Forward-IM-Sender": ""}))
	require.False(t, wantsIMSender(map[string]string{"X-Forward-IM-Sender": "0"}))
	require.False(t, wantsIMSender(map[string]string{"X-Forward-IM-Sender": " False "}))
	require.True(t, wantsIMSender(map[string]string{"X-Forward-IM-Sender": "1"}))
	require.True(t, wantsIMSender(map[string]string{"x-forward-im-sender": "true"}))
}

func TestIMSenderHeaders(t *testing.T) {
	im := func(id string) context.Context {
		return types.WithPrincipal(context.Background(), types.Principal{Type: types.PrincipalIMUser, ID: id})
	}
	require.Equal(t, map[string]string{
		"X-IM-Channel-Id": "chan-1",
		"X-IM-Platform":   "whatsapp",
		"X-IM-User-Id":    "60123456789",
	}, imSenderHeaders(im("7:chan-1:whatsapp:60123456789")))

	// the message the run answers travels with it
	withMessage := imSenderHeaders(types.WithIMMessageID(im("7:chan-1:whatsapp:60123456789"), "3EB0C767D0A5\r\n"))
	require.Equal(t, "3EB0C767D0A5", withMessage["X-IM-Message-Id"])
	require.Equal(t, "60123456789", withMessage["X-IM-User-Id"])
	// a message id without an IM user names nobody
	require.Nil(t, imSenderHeaders(types.WithIMMessageID(context.Background(), "3EB0C767D0A5")))

	// the user id keeps its own colons, and nothing HTTP refuses in a header gets through as written
	got := imSenderHeaders(im("7:chan-1:telegram:a b:c/é\r\nX-Injected: 1"))
	require.Equal(t, "a%20b:c%2F%C3%A9%0D%0AX-Injected:%201", got["X-IM-User-Id"])

	// not an IM user, or not the id withIMIdentity builds: nothing is said
	require.Nil(t, imSenderHeaders(context.Background()))
	require.Nil(t, imSenderHeaders(context.WithValue(context.Background(), types.UserIDContextKey, "u1")))
	require.Nil(t, imSenderHeaders(types.WithPrincipal(context.Background(),
		types.Principal{Type: types.PrincipalWebUser, ID: "7:chan-1:whatsapp:60123456789"})))
	require.Nil(t, imSenderHeaders(im("wecom:ch1:u1")))
	require.Nil(t, imSenderHeaders(im("7:chan-1:whatsapp:")))
}

// The headers travel with the tool call of an IM run to a service that asked
// for them, and with nothing else: not the handshake, not a service that did
// not ask, not a caller who is not an IM user.
func TestIMSenderReachesToolCalls(t *testing.T) {
	utils.SetSSRFWhitelistFromRaw("127.0.0.1")
	t.Cleanup(utils.ResetSSRFWhitelistForTest)

	imCtx := types.WithIMMessageID(types.WithPrincipal(context.Background(),
		types.Principal{Type: types.PrincipalIMUser, ID: "7:chan-1:whatsapp:60123456789"}), "3EB0C767D0A5")
	webCtx := context.WithValue(context.Background(), types.UserIDContextKey, "u1")

	for _, tc := range []struct {
		name    string
		headers types.MCPHeaders
		caller  context.Context
		want    string
	}{
		{"asked for, IM user", types.MCPHeaders{"X-Forward-IM-Sender": "1"}, imCtx, "60123456789"},
		{"not asked for, IM user", nil, imCtx, ""},
		{"asked for, web user", types.MCPHeaders{"X-Forward-IM-Sender": "1"}, webCtx, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := server.NewMCPServer("peer", "1")
			s.AddTool(sdk.Tool{Name: "publish", RawInputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
				func(context.Context, sdk.CallToolRequest) (*sdk.CallToolResult, error) {
					return sdk.NewToolResultText("ok"), nil
				})
			handler := server.NewStreamableHTTPServer(s, server.WithStateLess(true))

			var mu sync.Mutex
			seen := map[string][]http.Header{}
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					var request struct {
						Method string `json:"method"`
					}
					if json.Unmarshal(body, &request) == nil {
						mu.Lock()
						seen[request.Method] = append(seen[request.Method], r.Header.Clone())
						mu.Unlock()
					}
				}
				handler.ServeHTTP(w, r)
			}))
			defer httpServer.Close()

			c, err := NewMCPClient(&ClientConfig{Service: &types.MCPService{
				ID: "peer", Name: "peer", URL: &httpServer.URL, TransportType: types.MCPTransportHTTPStreamable,
				Headers:        tc.headers,
				AdvancedConfig: &types.MCPAdvancedConfig{Timeout: 3},
			}})
			require.NoError(t, err)
			defer func() { require.NoError(t, c.Disconnect()) }()

			// the handshake runs on the manager's own context, as in production
			setup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			require.NoError(t, c.Connect(setup))
			_, err = c.Initialize(setup)
			require.NoError(t, err)

			caller, stop := context.WithTimeout(tc.caller, 3*time.Second)
			defer stop()
			call, err := c.CallTool(caller, "publish", map[string]interface{}{})
			require.NoError(t, err)
			require.False(t, call.IsError)

			mu.Lock()
			defer mu.Unlock()
			require.NotEmpty(t, seen["tools/call"])
			for _, h := range seen["tools/call"] {
				require.Equal(t, tc.want, h.Get("X-IM-User-Id"))
				if tc.want != "" {
					require.Equal(t, "whatsapp", h.Get("X-IM-Platform"))
					require.Equal(t, "chan-1", h.Get("X-IM-Channel-Id"))
					require.Equal(t, "3EB0C767D0A5", h.Get("X-IM-Message-Id"))
				} else {
					require.Empty(t, h.Get("X-IM-Message-Id"))
				}
			}
			for method, list := range seen {
				if method == "tools/call" {
					continue
				}
				// CallTool may load the tool directory first; that request is the caller's too
				if method == "tools/list" {
					continue
				}
				for _, h := range list {
					require.Empty(t, h.Get("X-IM-User-Id"), "%s carried the sender", method)
				}
			}
		})
	}
}
