package im

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubBotImageFetch replaces the network fetch for the test's lifetime.
// images maps URL → payload; URLs not in the map fail.
func stubBotImageFetch(t *testing.T, images map[string][]byte) *[]string {
	t.Helper()
	orig := fetchBotImage
	var fetched []string
	fetchBotImage = func(_ context.Context, url string) ([]byte, string, error) {
		fetched = append(fetched, url)
		data, ok := images[url]
		if !ok {
			return nil, "", fmt.Errorf("stub: no such image")
		}
		return data, "image/png", nil
	}
	t.Cleanup(func() { fetchBotImage = orig })
	return &fetched
}

func TestExtractBotImageAttachmentsBasic(t *testing.T) {
	stubBotImageFetch(t, map[string][]byte{
		"https://assets.example.com/qr.png": []byte("png-bytes"),
	})
	content := "Scan this to download:\n\n![App QR](https://assets.example.com/qr.png)\n\nEnjoy!"
	cleaned, atts := extractBotImageAttachments(context.Background(), content, PlatformWhatsApp)
	if len(atts) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(atts))
	}
	att := atts[0]
	if att.Kind != MessageTypeImage || att.MimeType != "image/png" || att.FileName != "qr.png" || string(att.Data) != "png-bytes" {
		t.Fatalf("unexpected attachment: %+v", att)
	}
	if strings.Contains(cleaned, "![") || strings.Contains(cleaned, "qr.png") {
		t.Fatalf("markdown image not removed: %q", cleaned)
	}
	if cleaned != "Scan this to download:\n\nEnjoy!" {
		t.Fatalf("unexpected cleaned text: %q", cleaned)
	}
}

func TestExtractBotImageAttachmentsUnsupportedPlatform(t *testing.T) {
	fetched := stubBotImageFetch(t, map[string][]byte{"https://a/x.png": []byte("x")})
	content := "![x](https://a/x.png)"
	cleaned, atts := extractBotImageAttachments(context.Background(), content, PlatformTelegram)
	if atts != nil || cleaned != content {
		t.Fatalf("non-media platform must pass through, got %q atts=%d", cleaned, len(atts))
	}
	if len(*fetched) != 0 {
		t.Fatalf("must not fetch on unsupported platform, fetched %v", *fetched)
	}
}

func TestExtractBotImageAttachmentsFailureKeepsMarkdown(t *testing.T) {
	stubBotImageFetch(t, nil)
	content := "See ![qr](https://gone.example.com/x.png) here"
	cleaned, atts := extractBotImageAttachments(context.Background(), content, PlatformWhatsApp)
	if len(atts) != 0 {
		t.Fatalf("want 0 attachments, got %d", len(atts))
	}
	if cleaned != content {
		t.Fatalf("failed fetch must keep markdown, got %q", cleaned)
	}
}

func TestExtractBotImageAttachmentsDedupeAndCap(t *testing.T) {
	images := map[string][]byte{}
	for i := 0; i < 5; i++ {
		images[fmt.Sprintf("https://a/img%d.png", i)] = []byte{byte(i)}
	}
	fetched := stubBotImageFetch(t, images)
	var b strings.Builder
	b.WriteString("![dup](https://a/img0.png)\n![dup again](https://a/img0.png)\n")
	for i := 1; i < 5; i++ {
		fmt.Fprintf(&b, "![i%d](https://a/img%d.png)\n", i, i)
	}
	cleaned, atts := extractBotImageAttachments(context.Background(), b.String(), PlatformWhatsApp)
	if len(atts) != botReplyMaxImages {
		t.Fatalf("want %d attachments, got %d", botReplyMaxImages, len(atts))
	}
	if len(*fetched) != botReplyMaxImages {
		t.Fatalf("dedup/cap must bound fetches to %d, got %v", botReplyMaxImages, *fetched)
	}
	// Both occurrences of the deduped URL are removed; over-cap ones remain.
	if strings.Contains(cleaned, "img0.png") || strings.Contains(cleaned, "img2.png") {
		t.Fatalf("delivered refs must be removed: %q", cleaned)
	}
	if !strings.Contains(cleaned, "img3.png") || !strings.Contains(cleaned, "img4.png") {
		t.Fatalf("over-cap refs must stay as text: %q", cleaned)
	}
}

func TestExtractBotImageAttachmentsTitleAndNoExt(t *testing.T) {
	stubBotImageFetch(t, map[string][]byte{"https://a/download": []byte("x")})
	cleaned, atts := extractBotImageAttachments(
		context.Background(), `pre ![alt](https://a/download "Some title") post`, PlatformWhatsApp)
	if len(atts) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(atts))
	}
	if atts[0].FileName != "download.png" {
		t.Fatalf("extension not derived from mime: %q", atts[0].FileName)
	}
	if cleaned != "pre  post" {
		t.Fatalf("unexpected cleaned text: %q", cleaned)
	}
}

func TestExtractBotImageAttachmentsIntegrationAssetPath(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "demo", "assets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qr.png"), []byte("png-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEKNORA_INTEGRATIONS_DIR", base)

	content := "Scan:\n\n![QR](/api/v1/integration-assets/demo/qr.png)\n\nDone"
	cleaned, atts := extractBotImageAttachments(context.Background(), content, PlatformWhatsApp)
	if len(atts) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(atts))
	}
	if atts[0].MimeType != "image/png" || atts[0].FileName != "qr.png" || string(atts[0].Data) != "png-bytes" {
		t.Fatalf("unexpected attachment: %+v", atts[0])
	}
	if cleaned != "Scan:\n\nDone" {
		t.Fatalf("unexpected cleaned text: %q", cleaned)
	}
}

func TestBotImageFileName(t *testing.T) {
	cases := []struct{ url, mime, want string }{
		{"https://a/b/qr.png", "image/png", "qr.png"},
		{"https://a/", "image/jpeg", "image.jpg"},
		{"https://a/asset", "image/webp", "asset.webp"},
	}
	for _, c := range cases {
		if got := botImageFileName(c.url, c.mime); got != c.want {
			t.Errorf("botImageFileName(%q,%q)=%q want %q", c.url, c.mime, got, c.want)
		}
	}
}
