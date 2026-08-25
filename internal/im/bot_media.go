package im

// Bot outbound media: bot (auto-reply) answers can carry markdown images
// (`![alt](https://…)`) — typically KB-stored images rewritten to HTTP URLs by
// cleanIMContent, or fixed asset URLs the system prompt instructs the agent to
// emit. On platforms whose adapter supports outbound media (the same caps that
// gate operator manual replies), those images are downloaded server-side and
// delivered as real media messages instead of URL text.
//
// Only the non-streaming send path is wired: every media-capable platform
// today (WhatsApp) intentionally does not implement StreamSender, and the
// streaming Finalize contract has no attachment channel.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/utils"
)

const (
	// botReplyMaxImages bounds how many distinct images one bot reply may
	// deliver as media. Extra image references stay in the text as-is.
	botReplyMaxImages = 3
	// botImageFetchTimeout bounds one image download. The reply already took
	// a full QA round; a slow asset host must not stall delivery for long.
	botImageFetchTimeout = 15 * time.Second
)

// botImageMarkdownRe matches a markdown image whose target is an absolute
// http(s) URL, with an optional title. Relative and data: targets are left
// alone — only fetchable URLs can become attachments.
var botImageMarkdownRe = regexp.MustCompile(`!\[([^\]\n]*)\]\(\s*(https?://[^)\s]+?)(?:\s+"[^"]*")?\s*\)`)

var botImageHTTPClient = utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{
	Timeout:      botImageFetchTimeout,
	MaxRedirects: 5,
})

// fetchBotImage downloads one image URL and returns its payload and MIME
// type. Package variable so tests can stub the network.
var fetchBotImage = func(ctx context.Context, rawURL string) ([]byte, string, error) {
	if err := utils.ValidateURLForSSRF(rawURL); err != nil {
		return nil, "", fmt.Errorf("URL rejected by SSRF policy: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := botImageHTTPClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, manualReplyMaxImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("empty body")
	}
	if len(data) > manualReplyMaxImageBytes {
		return nil, "", fmt.Errorf("image exceeds %d MB", manualReplyMaxImageBytes>>20)
	}
	mimeType := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	if !strings.HasPrefix(mimeType, "image/") {
		// Header absent or generic (application/octet-stream, text/html error
		// pages, …): trust content sniffing instead.
		mimeType = strings.ToLower(http.DetectContentType(data))
	}
	if !strings.HasPrefix(mimeType, "image/") {
		return nil, "", fmt.Errorf("not an image: %s", mimeType)
	}
	return data, mimeType, nil
}

// outboundMediaSupported reports whether the platform's adapter understands
// ReplyMessage.Attachments (shared with the manual-reply media gate).
func outboundMediaSupported(platform Platform) bool {
	caps, ok := manualReplyPlatforms[strings.ToLower(string(platform))]
	return ok && caps.Media
}

// botImageFileName derives a display name for the attachment from the URL
// path, appending an extension from the MIME type when the path has none.
func botImageFileName(rawURL, mimeType string) string {
	name := ""
	if u, err := neturl.Parse(rawURL); err == nil {
		name = path.Base(u.Path)
	}
	if name == "" || name == "." || name == "/" {
		name = "image"
	}
	if !strings.Contains(name, ".") {
		switch mimeType {
		case "image/jpeg":
			name += ".jpg"
		case "image/gif":
			name += ".gif"
		case "image/webp":
			name += ".webp"
		default:
			name += ".png"
		}
	}
	return name
}

// blankRunRe collapses the blank runs left behind when image lines are
// removed from the reply text.
var blankRunRe = regexp.MustCompile(`\n[ \t]*\n(?:[ \t]*\n)+`)

// extractBotImageAttachments converts markdown image references in a final
// bot answer into ReplyAttachments for media-capable platforms, removing the
// delivered references from the text. References that fail to download (SSRF
// rejection, non-image content, oversize, network error) keep their markdown
// text — the status-quo URL fallback.
func extractBotImageAttachments(ctx context.Context, content string, platform Platform) (string, []*ReplyAttachment) {
	if !outboundMediaSupported(platform) {
		return content, nil
	}
	matches := botImageMarkdownRe.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return content, nil
	}

	// nil value = fetch attempted and failed (or over cap): keep the text.
	byURL := make(map[string]*ReplyAttachment)
	var attachments []*ReplyAttachment
	for _, m := range matches {
		url := m[2]
		if _, seen := byURL[url]; seen {
			continue
		}
		if len(attachments) >= botReplyMaxImages {
			byURL[url] = nil
			continue
		}
		fetchCtx, cancel := context.WithTimeout(ctx, botImageFetchTimeout)
		data, mimeType, err := fetchBotImage(fetchCtx, url)
		cancel()
		if err != nil {
			logger.Warnf(ctx, "[IM] bot reply image skipped (kept as text): url=%s err=%v", url, err)
			byURL[url] = nil
			continue
		}
		att := &ReplyAttachment{
			Kind:     MessageTypeImage,
			FileName: botImageFileName(url, mimeType),
			MimeType: mimeType,
			Data:     data,
		}
		byURL[url] = att
		attachments = append(attachments, att)
	}
	if len(attachments) == 0 {
		return content, nil
	}

	cleaned := botImageMarkdownRe.ReplaceAllStringFunc(content, func(ref string) string {
		m := botImageMarkdownRe.FindStringSubmatch(ref)
		if m == nil || byURL[m[2]] == nil {
			return ref
		}
		return ""
	})
	cleaned = strings.TrimSpace(blankRunRe.ReplaceAllString(cleaned, "\n\n"))
	logger.Infof(ctx, "[IM] bot reply carries %d image attachment(s) (platform=%s)", len(attachments), platform)
	return cleaned, attachments
}
