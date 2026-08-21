package whatsapp

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/im"
	"github.com/Tencent/WeKnora/internal/types"
)

// The credential errors surface in the channel UI and must fire before the
// factory ever touches the session store (db is nil here on purpose).
func TestFactoryCredentialErrors(t *testing.T) {
	factory := NewFactory(nil)
	ctx := context.Background()

	cases := []struct {
		name, creds, wantErr string
	}{
		{"unpaired channel", `{}`, "no device_jid"},
		{"invalid jid", `{"device_jid":"8613800138000:xx@s.whatsapp.net"}`, "invalid device_jid"},
		{"broken json", `{oops`, "parse whatsapp credentials"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channel := &im.IMChannel{ID: "ch-factory", Credentials: types.JSON(tc.creds)}
			_, _, err := factory(ctx, channel, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("abcdefghij"); got != "abcdefgh" {
		t.Errorf("shortID long = %q", got)
	}
	if got := shortID("ab"); got != "ab" {
		t.Errorf("shortID short = %q", got)
	}
}
