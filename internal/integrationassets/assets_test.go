package integrationassets

import (
	"os"
	"path/filepath"
	"testing"
)

func setupAssets(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "demo", "assets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qr.png"), []byte("png-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "demo", "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEKNORA_INTEGRATIONS_DIR", base)
}

func TestResolveURLPath(t *testing.T) {
	setupAssets(t)
	data, contentType, err := ResolveURLPath(URLPrefix + "demo/qr.png")
	if err != nil || string(data) != "png-bytes" || contentType != "image/png" {
		t.Fatalf("got data=%q type=%q err=%v", data, contentType, err)
	}
}

func TestResolveRejectsBadPaths(t *testing.T) {
	setupAssets(t)
	bad := []string{
		URLPrefix + "demo/../demo/qr.png", // traversal
		URLPrefix + "..%2Fdemo/qr.png",    // encoded-ish junk segment
		URLPrefix + "demo/secret.txt",     // extension not whitelisted
		URLPrefix + "demo/.hidden.png",    // dotfile
		URLPrefix + "demo/missing.png",    // absent
		URLPrefix + "demo",                // no file part
		"/api/v1/other/demo/qr.png",       // wrong prefix
	}
	for _, p := range bad {
		if _, _, err := ResolveURLPath(p); err == nil {
			t.Errorf("expected rejection for %q", p)
		}
	}
}
