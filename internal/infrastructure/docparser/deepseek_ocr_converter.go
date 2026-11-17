package docparser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

const (
	// deepSeekOCRDefaultBaseURL is DeepSeek's API host. The OCR endpoint is
	// {base}/v1/tools/ocr; a base that already ends in /v1 is accepted.
	deepSeekOCRDefaultBaseURL = "https://api.deepseek.com"

	// DeepSeekOCRPurposeText recognises text: a Word or PowerPoint file with
	// no pictures has its text extracted, every other file is read page by
	// page. DeepSeekOCRPurposeRichLayout reads every file page by page and
	// marks each block with its kind and position (removed here).
	DeepSeekOCRPurposeText       = "text_recognition"
	DeepSeekOCRPurposeRichLayout = "rich_layout_recognition"

	// deepSeekOCRTimeout bounds one request. The API ends a request 180 s after
	// the upload starts, upload and recognition included.
	deepSeekOCRTimeout = 200 * time.Second

	// deepSeekOCRAttempts is how many times a file is sent when every page
	// comes back without text. The API sometimes answers 200 with every page
	// empty (finish_reason "stop", no output); a later request for the same
	// file has returned its text, once only on the third try.
	deepSeekOCRAttempts = 3

	// maxDeepSeekOCRResponseBytes bounds the reply: the text of up to 1000
	// pages, as JSON.
	maxDeepSeekOCRResponseBytes = 64 << 20
)

// DeepSeekOCRReader reads a document or a picture with DeepSeek's OCR API,
// which returns the text of each page. DeepSeek does not keep the file in its
// Files API and does not give it to a chat model.
type DeepSeekOCRReader struct {
	apiKey  string
	baseURL string
	purpose string
	timeout time.Duration
}

// NewDeepSeekOCRReader creates a reader from ParserEngineOverrides.
func NewDeepSeekOCRReader(overrides map[string]string) *DeepSeekOCRReader {
	purpose := strings.TrimSpace(overrides["deepseek_ocr_purpose"])
	if purpose != DeepSeekOCRPurposeRichLayout {
		purpose = DeepSeekOCRPurposeText
	}
	base := strings.TrimRight(stringOr(overrides["deepseek_ocr_base_url"], deepSeekOCRDefaultBaseURL), "/")
	return &DeepSeekOCRReader{
		apiKey:  strings.TrimSpace(overrides["deepseek_ocr_api_key"]),
		baseURL: strings.TrimSuffix(base, "/v1"),
		purpose: purpose,
		timeout: requestTimeoutFromEnv("WEKNORA_DEEPSEEK_OCR_TIMEOUT", deepSeekOCRTimeout),
	}
}

func (c *DeepSeekOCRReader) Read(ctx context.Context, req *types.ReadRequest) (*types.ReadResult, error) {
	if c.apiKey == "" {
		return &types.ReadResult{Error: "DeepSeek OCR API key is not configured"}, nil
	}
	if err := utils.ValidateURLForSSRF(c.baseURL); err != nil {
		return &types.ReadResult{Error: fmt.Sprintf("DeepSeek OCR base URL blocked by SSRF policy: %v", err)}, nil
	}
	if len(req.FileContent) == 0 {
		return &types.ReadResult{Error: "no file content provided"}, nil
	}
	fileName := req.FileName
	if fileName == "" {
		ext := strings.TrimPrefix(req.FileType, ".")
		if ext == "" {
			ext = "pdf"
		}
		fileName = "document." + ext
	}

	var (
		pages     []deepSeekOCRPage
		text      string
		truncated []int
	)
	for attempt := 1; attempt <= deepSeekOCRAttempts; attempt++ {
		var err error
		if pages, err = c.recognize(ctx, fileName, req.FileContent); err != nil {
			return nil, err
		}
		if text, truncated = joinDeepSeekOCRPages(pages); text != "" {
			break
		}
		logger.Warnf(ctx, "[DeepSeek OCR] attempt %d/%d: no text in %d page(s)", attempt, deepSeekOCRAttempts, len(pages))
	}
	if text == "" {
		return nil, fmt.Errorf("DeepSeek OCR found no text in %d page(s) after %d attempts", len(pages), deepSeekOCRAttempts)
	}
	metadata := map[string]string{
		"parser":  DeepSeekOCREngineName,
		"pages":   strconv.Itoa(len(pages)),
		"purpose": c.purpose,
	}
	if len(truncated) > 0 {
		list := make([]string, len(truncated))
		for i, page := range truncated {
			list[i] = strconv.Itoa(page)
		}
		metadata["truncated_pages"] = strings.Join(list, ",")
		logger.Warnf(ctx, "[DeepSeek OCR] page(s) %s stopped at the model's output limit; their text is incomplete",
			metadata["truncated_pages"])
	}

	md, imageRefs := ensureOriginalImageRef(req, NormalizeHTMLTables(text), nil)
	logger.Infof(ctx, "[DeepSeek OCR] parsed: pages=%d chars=%d", len(pages), len(md))
	return &types.ReadResult{MarkdownContent: md, ImageRefs: imageRefs, Metadata: metadata}, nil
}

type deepSeekOCRPage struct {
	Index        int    `json:"index"`
	Text         string `json:"text"`
	FinishReason string `json:"finish_reason"`
}

type deepSeekOCRResponse struct {
	Pages []deepSeekOCRPage `json:"pages"`
	Error *deepSeekOCRError `json:"error"`
}

type deepSeekOCRError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

// recognize sends the file once. The form has to carry purpose before file.
// While it works the API sends newlines to keep the connection open; they are
// whitespace before the JSON. Once recognition has started the status is 200,
// and a failure comes as {"error": {...}} in the body.
func (c *DeepSeekOCRReader) recognize(ctx context.Context, fileName string, content []byte) ([]deepSeekOCRPage, error) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("purpose", c.purpose); err != nil {
		return nil, fmt.Errorf("DeepSeek OCR form: %w", err)
	}
	part, err := form.CreateFormFile("file", fileName)
	if err != nil {
		return nil, fmt.Errorf("DeepSeek OCR form: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return nil, fmt.Errorf("DeepSeek OCR form: %w", err)
	}
	if err := form.Close(); err != nil {
		return nil, fmt.Errorf("DeepSeek OCR form: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/tools/ocr", &body)
	if err != nil {
		return nil, fmt.Errorf("DeepSeek OCR request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", form.FormDataContentType())

	client := utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{Timeout: c.timeout, MaxRedirects: 5})
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("DeepSeek OCR request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DeepSeek OCR: HTTP %d: %s", resp.StatusCode, deepSeekOCRErrorText(readErrorPreview(resp.Body)))
	}
	data, err := readCapped(resp.Body, maxDeepSeekOCRResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("DeepSeek OCR reply: %w", err)
	}
	var parsed deepSeekOCRResponse
	if err := json.Unmarshal(bytes.TrimSpace(data), &parsed); err != nil {
		return nil, fmt.Errorf("DeepSeek OCR reply is not JSON: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("DeepSeek OCR: %s", strings.TrimSpace(parsed.Error.Message))
	}
	return parsed.Pages, nil
}

// deepSeekOCRErrorText is the message of an {"error": {...}} body, or the start
// of any other body.
func deepSeekOCRErrorText(body string) string {
	var parsed deepSeekOCRResponse
	if json.Unmarshal([]byte(strings.TrimSpace(body)), &parsed) == nil && parsed.Error != nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	return strings.TrimSpace(body)
}

// Page-by-page recognition can leave layout markers in a page's text:
// "<｜ref｜>kind<｜/ref｜><｜box｜>[[x1, y1, x2, y2]]<｜/box｜> words". The ref
// holds the block's kind (text, title, image), not its words, so both markers
// go and the words after them stay. The API writes the bars as U+FF5C; ASCII
// bars are taken too.
var (
	deepSeekOCRRefRe    = regexp.MustCompile(`<[｜|]ref[｜|]>[\s\S]*?<[｜|]/ref[｜|]>[ \t]*`)
	deepSeekOCRBoxRe    = regexp.MustCompile(`<[｜|]box[｜|]>[\s\S]*?<[｜|]/box[｜|]>[ \t]*`)
	deepSeekOCRTokenRe  = regexp.MustCompile(`<[｜|][^｜|<>\n]{1,40}[｜|]>`)
	deepSeekOCRBlanksRe = regexp.MustCompile(`\n{3,}`)
)

func cleanDeepSeekOCRText(text string) string {
	text = deepSeekOCRRefRe.ReplaceAllString(text, "")
	text = deepSeekOCRBoxRe.ReplaceAllString(text, "")
	text = deepSeekOCRTokenRe.ReplaceAllString(text, "")
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.TrimSpace(deepSeekOCRBlanksRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// joinDeepSeekOCRPages joins the pages' text in page order, a blank line
// between pages, and lists (from 1) the pages that stopped at the output limit.
func joinDeepSeekOCRPages(pages []deepSeekOCRPage) (string, []int) {
	ordered := append([]deepSeekOCRPage(nil), pages...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })
	parts := make([]string, 0, len(ordered))
	var truncated []int
	for _, page := range ordered {
		if page.FinishReason == "length" {
			truncated = append(truncated, page.Index+1)
		}
		if text := cleanDeepSeekOCRText(page.Text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n"), truncated
}
