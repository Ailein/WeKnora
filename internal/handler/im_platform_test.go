package handler

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/im"
)

// The channel-creation endpoint rejects any platform without a registered
// adapter factory, so this set must track the factories wired in the container.
func TestValidIMPlatforms_CoversLark(t *testing.T) {
	want := []string{
		"wecom", "feishu", "lark", "slack", "telegram",
		"dingtalk", "mattermost", "wechat", "qqbot", "whatsapp",
	}
	for _, platform := range want {
		if !validIMPlatforms[platform] {
			t.Errorf("platform %q is not accepted", platform)
		}
	}
	if validIMPlatforms["nonsense"] {
		t.Error("unknown platform is accepted")
	}
}

// The 400 message is derived from validIMPlatforms; it must not drift as
// platforms are added.
func TestInvalidIMPlatformError_ListsEveryPlatform(t *testing.T) {
	for platform := range validIMPlatforms {
		if !strings.Contains(invalidIMPlatformError, "'"+platform+"'") {
			t.Errorf("error message omits %q: %s", platform, invalidIMPlatformError)
		}
	}
}

// Platform-fixed transports must hold on update as well as create: flipping a
// whatsapp channel to webhook would open its socket on every replica (webhook
// mode skips leader election) and corrupt the WhatsApp session.
func TestEnforcePlatformFixedModes(t *testing.T) {
	cases := []struct {
		platform, wantMode, wantOutput string
	}{
		{"wechat", "longpoll", "full"},
		{"whatsapp", "websocket", "full"},
	}
	for _, tc := range cases {
		channel := &im.IMChannel{Platform: tc.platform, Mode: "webhook", OutputMode: "stream"}
		enforcePlatformFixedModes(channel)
		if channel.Mode != tc.wantMode || channel.OutputMode != tc.wantOutput {
			t.Errorf("%s: got mode=%q output=%q, want %q/%q",
				tc.platform, channel.Mode, channel.OutputMode, tc.wantMode, tc.wantOutput)
		}
	}

	// Free-mode platforms must pass through untouched.
	channel := &im.IMChannel{Platform: "slack", Mode: "webhook", OutputMode: "stream"}
	enforcePlatformFixedModes(channel)
	if channel.Mode != "webhook" || channel.OutputMode != "stream" {
		t.Errorf("slack channel modified: mode=%q output=%q", channel.Mode, channel.OutputMode)
	}
}
