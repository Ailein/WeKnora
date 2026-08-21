package whatsapp

import (
	"strings"
	"testing"
)

func TestToWhatsAppMarkup(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bold stars", "this is **bold** text", "this is *bold* text"},
		{"bold underscore", "this is __bold__ text", "this is *bold* text"},
		{"strike", "~~gone~~", "~gone~"},
		{"heading", "## Section title", "*Section title*"},
		{"link", "see [docs](https://example.com) here", "see docs (https://example.com) here"},
		{"italic untouched", "keep _italic_ as is", "keep _italic_ as is"},
		{"inline code preserved", "use `f(*args, **kwargs)` or `g(**opts)`", "use `f(*args, **kwargs)` or `g(**opts)`"},
		{"inline code with strike/link", "run `~~x~~` and `[a](b)` please", "run `~~x~~` and `[a](b)` please"},
		{"bold outside inline code", "**bold** and `**code**`", "*bold* and `**code**`"},
		{"inline code in heading", "## Use `**kwargs` here", "*Use `**kwargs` here*"},
		// The heading itself becomes the bold span; nested bold markers must
		// be stripped, not converted, or WhatsApp shows literal asterisks.
		{"bold inside heading stripped", "## **Bold** title", "*Bold title*"},
		{"link inside heading", "## See [docs](https://example.com)", "*See docs (https://example.com)*"},
		{"strike inside heading", "## ~~Old~~ New", "*~Old~ New*"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toWhatsAppMarkup(tc.in); got != tc.want {
				t.Errorf("toWhatsAppMarkup(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Fenced code block contents must not be rewritten, and the language tag on
// the opening fence is dropped.
func TestToWhatsAppMarkup_CodeBlockPreserved(t *testing.T) {
	in := "```go\nx := \"**not bold**\"\n# not a heading\n```"
	got := toWhatsAppMarkup(in)
	if !strings.Contains(got, "**not bold**") {
		t.Errorf("code block content was rewritten: %q", got)
	}
	if strings.Contains(got, "```go") {
		t.Errorf("language tag not dropped: %q", got)
	}
	if !strings.Contains(got, "# not a heading") {
		t.Errorf("heading rewritten inside code block: %q", got)
	}
}

func TestSplitMessage(t *testing.T) {
	if got := splitMessage("short", 100); len(got) != 1 || got[0] != "short" {
		t.Errorf("short message split unexpectedly: %v", got)
	}
	if got := splitMessage("  ", 100); got != nil {
		t.Errorf("blank message should produce no segments: %v", got)
	}

	// Prefers paragraph boundaries in the second half of the window.
	long := strings.Repeat("a", 60) + "\n\n" + strings.Repeat("b", 60)
	got := splitMessage(long, 80)
	if len(got) != 2 || got[0] != strings.Repeat("a", 60) || got[1] != strings.Repeat("b", 60) {
		t.Errorf("paragraph split failed: %d segments %v", len(got), got)
	}

	// Multi-byte runes must not be cut mid-character.
	cjk := strings.Repeat("中文消息内容", 100) // 600 runes
	for i, seg := range splitMessage(cjk, 250) {
		if !strings.HasPrefix(seg, "中") && !strings.HasPrefix(seg, "文") &&
			!strings.HasPrefix(seg, "消") && !strings.HasPrefix(seg, "息") &&
			!strings.HasPrefix(seg, "内") && !strings.HasPrefix(seg, "容") {
			t.Errorf("segment %d starts with invalid rune: %q", i, seg[:3])
		}
		if len([]rune(seg)) > 250 {
			t.Errorf("segment %d exceeds limit: %d runes", i, len([]rune(seg)))
		}
	}
}

func TestParseAllowFrom(t *testing.T) {
	if got := parseAllowFrom(""); len(got) != 0 {
		t.Errorf("empty allow_from should be fail-closed, got %v", got)
	}
	got := parseAllowFrom("+86 138-0013-8000, 15551234567 , *")
	if !got["*"] || !got["8613800138000"] || !got["15551234567"] {
		t.Errorf("parseAllowFrom normalized set wrong: %v", got)
	}

	a := &Adapter{allowFrom: parseAllowFrom("+8613800138000")}
	if !a.dmAllowed("8613800138000") {
		t.Error("normalized number should be allowed")
	}
	if a.dmAllowed("999") {
		t.Error("unknown number should be rejected")
	}
	open := &Adapter{allowFrom: parseAllowFrom("*")}
	if !open.dmAllowed("999") {
		t.Error("wildcard should allow everyone")
	}
	closed := &Adapter{allowFrom: parseAllowFrom("")}
	if closed.dmAllowed("8613800138000") {
		t.Error("empty allowlist must reject all DMs (fail-closed)")
	}
}
