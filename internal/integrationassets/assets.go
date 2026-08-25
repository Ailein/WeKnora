// Package integrationassets resolves public static assets shipped by
// repo integrations (integrations/<name>/assets/<file>). Two consumers share
// it: the unauthenticated /api/v1/integration-assets router (browser <img>
// tags cannot attach credentials) and the IM bot-media path, which converts
// markdown references to these assets into outbound media attachments.
// Everything under an integration's assets/ directory is therefore public by
// design — only ship shareable marketing assets (QR codes, logos) there.
package integrationassets

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// URLPrefix is the public route prefix; markdown image targets starting with
// it are treated as integration-asset references.
const URLPrefix = "/api/v1/integration-assets/"

// segmentRe allows plain file-name-ish segments only. The leading class
// rejects dotfiles (and thus ".."); the body has no path separators.
var segmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// contentTypes doubles as the served-extension whitelist.
var contentTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
}

// BaseDir is the integrations root (compose mounts ./integrations read-only
// into the app container; the default resolves under the app workdir).
func BaseDir() string {
	if dir := os.Getenv("WEKNORA_INTEGRATIONS_DIR"); dir != "" {
		return dir
	}
	return "./integrations"
}

// Resolve validates an (integration, file) pair and returns the asset's
// on-disk path and content type without reading it.
func Resolve(integration, file string) (string, string, error) {
	if !segmentRe.MatchString(integration) || !segmentRe.MatchString(file) {
		return "", "", fmt.Errorf("invalid asset path segment")
	}
	contentType, ok := contentTypes[strings.ToLower(filepath.Ext(file))]
	if !ok {
		return "", "", fmt.Errorf("asset extension not allowed")
	}
	fullPath := filepath.Join(BaseDir(), integration, "assets", file)
	info, err := os.Stat(fullPath)
	if err != nil || info.IsDir() {
		return "", "", fmt.Errorf("asset not found")
	}
	return fullPath, contentType, nil
}

// ResolveURLPath resolves a "/api/v1/integration-assets/<integration>/<file>"
// reference and returns the asset bytes and content type.
func ResolveURLPath(urlPath string) ([]byte, string, error) {
	rest, ok := strings.CutPrefix(urlPath, URLPrefix)
	if !ok {
		return nil, "", fmt.Errorf("not an integration-asset path")
	}
	integration, file, ok := strings.Cut(rest, "/")
	if !ok {
		return nil, "", fmt.Errorf("malformed integration-asset path")
	}
	fullPath, contentType, err := Resolve(integration, file)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, "", err
	}
	return data, contentType, nil
}
