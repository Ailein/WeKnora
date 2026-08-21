package whatsapp

import (
	"regexp"
	"strconv"
	"strings"
)

// WhatsApp renders its own lightweight markup: *bold*, _italic_, ~strike~,
// ```code blocks```, `inline code`, "- " bullets, "1. " ordered lists and
// "> " quotes. LLM answers arrive as standard Markdown, so the common
// constructs that differ are rewritten here; unknown constructs pass through.

var (
	mdBoldStars      = regexp.MustCompile(`\*\*(.+?)\*\*`)
	mdBoldUnderscore = regexp.MustCompile(`__(.+?)__`)
	mdStrike         = regexp.MustCompile(`~~(.+?)~~`)
	mdHeading        = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*)$`)
	mdLink           = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	mdInlineCode     = regexp.MustCompile("`[^`\n]*`")
)

// protectInlineCode swaps inline `code` spans for placeholders so the markup
// rewrites cannot mangle their contents (e.g. the ** pair in `f(*a, **kw)`).
func protectInlineCode(line string) (string, []string) {
	if !strings.Contains(line, "`") {
		return line, nil
	}
	var spans []string
	line = mdInlineCode.ReplaceAllStringFunc(line, func(m string) string {
		spans = append(spans, m)
		return "\x00" + strconv.Itoa(len(spans)-1) + "\x00"
	})
	return line, spans
}

func restoreInlineCode(line string, spans []string) string {
	for i, span := range spans {
		line = strings.Replace(line, "\x00"+strconv.Itoa(i)+"\x00", span, 1)
	}
	return line
}

// toWhatsAppMarkup converts standard Markdown to WhatsApp markup.
// Fenced code block contents are preserved verbatim (the ``` fences
// themselves are valid WhatsApp markup, but language tags are dropped).
func toWhatsAppMarkup(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if !inFence {
				// Drop the language tag: "```go" → "```".
				line = strings.Repeat(" ", indentOf(line)) + "```"
			}
			inFence = !inFence
			out = append(out, line)
			continue
		}
		if inFence {
			out = append(out, line)
			continue
		}
		var codeSpans []string
		line, codeSpans = protectInlineCode(line)
		if m := mdHeading.FindStringSubmatch(line); m != nil {
			// The whole heading becomes one bold line, so nested bold markers
			// are stripped rather than converted — "*a *b* c*" renders as
			// literal asterisks on WhatsApp. Strike and links convert as usual.
			inner := mdBoldStars.ReplaceAllString(m[1], "$1")
			inner = mdBoldUnderscore.ReplaceAllString(inner, "$1")
			inner = mdStrike.ReplaceAllString(inner, "~$1~")
			inner = mdLink.ReplaceAllString(inner, "$1 ($2)")
			out = append(out, "*"+strings.TrimSpace(restoreInlineCode(inner, codeSpans))+"*")
			continue
		}
		line = mdBoldStars.ReplaceAllString(line, "*$1*")
		line = mdBoldUnderscore.ReplaceAllString(line, "*$1*")
		line = mdStrike.ReplaceAllString(line, "~$1~")
		line = mdLink.ReplaceAllString(line, "$1 ($2)")
		out = append(out, restoreInlineCode(line, codeSpans))
	}
	return strings.Join(out, "\n")
}

func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
}

// maxSegmentRunes keeps each outgoing message comfortably under WhatsApp's
// 4096-character hard limit.
const maxSegmentRunes = 4000

// splitMessage splits text into segments of at most limit runes, preferring
// paragraph boundaries, then line boundaries, then a hard cut. Boundary cuts
// are only taken in the second half of the window to avoid tiny fragments.
func splitMessage(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	var segments []string
	runes := []rune(text)
	for len(runes) > limit {
		window := string(runes[:limit])
		cut := limit
		// strings.LastIndex returns a byte offset; "\n" is ASCII so the
		// offset is a rune boundary and window[:idx] converts cleanly.
		if idx := strings.LastIndex(window, "\n\n"); idx >= 0 && len([]rune(window[:idx])) >= limit/2 {
			cut = len([]rune(window[:idx]))
		} else if idx := strings.LastIndex(window, "\n"); idx >= 0 && len([]rune(window[:idx])) >= limit/2 {
			cut = len([]rune(window[:idx]))
		}
		if segment := strings.TrimSpace(string(runes[:cut])); segment != "" {
			segments = append(segments, segment)
		}
		runes = []rune(strings.TrimSpace(string(runes[cut:])))
	}
	if rest := strings.TrimSpace(string(runes)); rest != "" {
		segments = append(segments, rest)
	}
	return segments
}
