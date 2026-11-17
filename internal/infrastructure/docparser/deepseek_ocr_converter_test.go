package docparser

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deepSeekOCRSent is what the fake OCR API was sent last.
type deepSeekOCRSent struct {
	mu       sync.Mutex
	auth     string
	fields   []string
	purpose  string
	fileName string
	content  []byte
}

func (s *deepSeekOCRSent) snapshot() deepSeekOCRSent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return deepSeekOCRSent{auth: s.auth, fields: append([]string(nil), s.fields...), purpose: s.purpose, fileName: s.fileName, content: s.content}
}

// newDeepSeekOCRServer stands in for POST /v1/tools/ocr. The n-th request gets
// replies[n-1], and every later one the last reply.
func newDeepSeekOCRServer(t *testing.T, status int, replies ...string) (*DeepSeekOCRReader, *atomic.Int32, *deepSeekOCRSent) {
	t.Helper()
	allowLoopbackSSRF(t)
	var calls atomic.Int32
	sent := &deepSeekOCRSent{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tools/ocr" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		form, err := r.MultipartReader()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sent.mu.Lock()
		sent.auth = r.Header.Get("Authorization")
		sent.fields = nil
		for {
			part, err := form.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				sent.mu.Unlock()
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			data, _ := io.ReadAll(part)
			sent.fields = append(sent.fields, part.FormName())
			switch part.FormName() {
			case "purpose":
				sent.purpose = string(data)
			case "file":
				sent.fileName = part.FileName()
				sent.content = data
			}
		}
		sent.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, replies[min(n, len(replies))-1])
	}))
	t.Cleanup(server.Close)
	reader := NewDeepSeekOCRReader(map[string]string{
		"deepseek_ocr_api_key":  "test-key",
		"deepseek_ocr_base_url": server.URL,
	})
	return reader, &calls, sent
}

func pdfRequest() *types.ReadRequest {
	return &types.ReadRequest{FileContent: []byte("%PDF-1.4 scanned"), FileName: "ad.pdf", FileType: "pdf"}
}

const emptyDeepSeekOCRPage = `{"page_count":1,"pages":[{"index":0,"text":"","finish_reason":"stop"}]}`

func TestDeepSeekOCRReaderSendsPurposeFirstAndJoinsPagesInOrder(t *testing.T) {
	// While it works the API sends newlines, then the JSON.
	reply := "\n\n\n" + `{"id":"ocr-1","object":"ocr.result","purpose":"text_recognition","page_count":2,"pages":[` +
		`{"index":1,"text":"Page two: WhatsApp 012-345 6789","finish_reason":"stop"},` +
		`{"index":0,"text":"# PROMOTER\n<table><tr><td>Role</td><td>Pay</td></tr><tr><td>Usher</td><td>RM 100/day</td></tr></table>","finish_reason":"stop"}]}`
	reader, calls, sent := newDeepSeekOCRServer(t, http.StatusOK, reply)

	result, err := reader.Read(context.Background(), pdfRequest())
	require.NoError(t, err)
	require.Empty(t, result.Error)
	assert.EqualValues(t, 1, calls.Load())

	got := sent.snapshot()
	assert.Equal(t, "Bearer test-key", got.auth)
	assert.Equal(t, []string{"purpose", "file"}, got.fields, "the API wants purpose before file")
	assert.Equal(t, DeepSeekOCRPurposeText, got.purpose)
	assert.Equal(t, "ad.pdf", got.fileName)
	assert.Equal(t, "%PDF-1.4 scanned", string(got.content))

	md := result.MarkdownContent
	assert.True(t, strings.HasPrefix(md, "# PROMOTER"), md)
	assert.Less(t, strings.Index(md, "Usher"), strings.Index(md, "Page two"), "pages in index order")
	assert.Contains(t, md, "RM 100/day")
	assert.NotContains(t, strings.ToLower(md), "<table", "HTML tables become Markdown")
	assert.Equal(t, DeepSeekOCREngineName, result.Metadata["parser"])
	assert.Equal(t, "2", result.Metadata["pages"])
	assert.Empty(t, result.Metadata["truncated_pages"])
	assert.Empty(t, result.ImageRefs)
}

func TestDeepSeekOCRReaderRetriesWhenEveryPageIsEmpty(t *testing.T) {
	full := `{"page_count":1,"pages":[{"index":0,"text":"RM 14/hour","finish_reason":"stop"}]}`
	reader, calls, _ := newDeepSeekOCRServer(t, http.StatusOK, emptyDeepSeekOCRPage, emptyDeepSeekOCRPage, full)

	result, err := reader.Read(context.Background(), pdfRequest())
	require.NoError(t, err)
	assert.Equal(t, "RM 14/hour", result.MarkdownContent)
	assert.EqualValues(t, 3, calls.Load())
}

func TestDeepSeekOCRReaderGivesUpWhenEveryAttemptIsEmpty(t *testing.T) {
	reader, calls, _ := newDeepSeekOCRServer(t, http.StatusOK, emptyDeepSeekOCRPage)

	_, err := reader.Read(context.Background(), pdfRequest())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "found no text")
	assert.EqualValues(t, deepSeekOCRAttempts, calls.Load())
}

func TestDeepSeekOCRReaderReportsTheAPIsError(t *testing.T) {
	t.Run("in the body, once recognition has started", func(t *testing.T) {
		reader, calls, _ := newDeepSeekOCRServer(t, http.StatusOK,
			"\n\n"+`{"error":{"message":"OCR did not finish in time","type":"gateway_timeout_error","param":null,"code":"gateway_timeout_error"}}`)
		_, err := reader.Read(context.Background(), pdfRequest())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "OCR did not finish in time")
		assert.EqualValues(t, 1, calls.Load(), "an error is not retried")
	})
	t.Run("as an HTTP status, before it started", func(t *testing.T) {
		reader, calls, _ := newDeepSeekOCRServer(t, http.StatusPaymentRequired, `{"error":{"message":"Insufficient Balance"}}`)
		_, err := reader.Read(context.Background(), pdfRequest())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HTTP 402")
		assert.Contains(t, err.Error(), "Insufficient Balance")
		assert.EqualValues(t, 1, calls.Load())
	})
}

func TestDeepSeekOCRReaderMarksPagesCutAtTheOutputLimit(t *testing.T) {
	reader, _, _ := newDeepSeekOCRServer(t, http.StatusOK,
		`{"pages":[{"index":0,"text":"first","finish_reason":"length"},{"index":1,"text":"second","finish_reason":"stop"},{"index":2,"text":"third","finish_reason":"length"}]}`)
	result, err := reader.Read(context.Background(), pdfRequest())
	require.NoError(t, err)
	assert.Equal(t, "first\n\nsecond\n\nthird", result.MarkdownContent)
	assert.Equal(t, "1,3", result.Metadata["truncated_pages"])
}

func TestDeepSeekOCRReaderKeepsAPictureAsItsOwnImage(t *testing.T) {
	reader, _, sent := newDeepSeekOCRServer(t, http.StatusOK, `{"pages":[{"index":0,"text":"EARN RM 130/DAY","finish_reason":"stop"}]}`)
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}

	result, err := reader.Read(context.Background(), &types.ReadRequest{FileContent: jpeg, FileName: "photo.jpg", FileType: "jpg"})
	require.NoError(t, err)
	assert.Equal(t, "![photo.jpg](images/photo.jpg)\n\nEARN RM 130/DAY", result.MarkdownContent)
	require.Len(t, result.ImageRefs, 1)
	assert.Equal(t, "photo.jpg", result.ImageRefs[0].Filename)
	assert.Equal(t, "photo.jpg", sent.snapshot().fileName)
}

func TestDeepSeekOCRReaderWithoutAKeySaysSoInTheResult(t *testing.T) {
	result, err := NewDeepSeekOCRReader(nil).Read(context.Background(), pdfRequest())
	require.NoError(t, err)
	assert.Equal(t, "DeepSeek OCR API key is not configured", result.Error)
}

func TestNewDeepSeekOCRReaderConfig(t *testing.T) {
	t.Setenv("WEKNORA_DEEPSEEK_OCR_TIMEOUT", "")
	custom := NewDeepSeekOCRReader(map[string]string{
		"deepseek_ocr_api_key":  " key ",
		"deepseek_ocr_base_url": "https://api.deepseek.com/v1/",
		"deepseek_ocr_purpose":  DeepSeekOCRPurposeRichLayout,
	})
	assert.Equal(t, "key", custom.apiKey)
	assert.Equal(t, "https://api.deepseek.com", custom.baseURL, "a base with /v1, as the chat API's, is accepted")
	assert.Equal(t, DeepSeekOCRPurposeRichLayout, custom.purpose)

	defaults := NewDeepSeekOCRReader(map[string]string{"deepseek_ocr_purpose": "something else"})
	assert.Equal(t, deepSeekOCRDefaultBaseURL, defaults.baseURL)
	assert.Equal(t, DeepSeekOCRPurposeText, defaults.purpose)
	assert.Equal(t, deepSeekOCRTimeout, defaults.timeout)

	t.Setenv("WEKNORA_DEEPSEEK_OCR_TIMEOUT", "90s")
	assert.Equal(t, 90*time.Second, NewDeepSeekOCRReader(nil).timeout)
}

func TestCleanDeepSeekOCRTextRemovesLayoutMarkers(t *testing.T) {
	// rich_layout_recognition output: the ref holds the block's kind, not its words
	raw := "<｜ref｜>image<｜/ref｜><｜box｜>[[117,127,153,156]]<｜/box｜>  \n" +
		"<｜ref｜>text<｜/ref｜><｜box｜>[[160,127,561,156]]<｜/box｜> LUMINA CREW AGENCY \n" +
		"<｜ref｜>sub_title<｜/ref｜><｜box｜>[[148,309,414,351]]<｜/box｜> ### RM 14/hour \n" +
		"<|ref|>text<|/ref|><|box|>[[1, 2, 3, 4]]<|/box|> WA 019-876 5432\n\n\n\nLimited slots!<｜end▁of▁sentence｜>"
	assert.Equal(t, "LUMINA CREW AGENCY\n### RM 14/hour\nWA 019-876 5432\n\nLimited slots!", cleanDeepSeekOCRText(raw))
	// text_recognition output has no markers and is kept as it is
	assert.Equal(t, "# WE ARE HIRING\n\n## RM 14/hour", cleanDeepSeekOCRText("# WE ARE HIRING\n\n## RM 14/hour\n"))
}

func TestDeepSeekOCREngineIsRegistered(t *testing.T) {
	reader, err := NewReader(context.Background(), DeepSeekOCREngineName, "docx", false,
		ReaderDeps{Overrides: map[string]string{"deepseek_ocr_api_key": "key"}})
	require.NoError(t, err)
	_, ok := reader.(*DeepSeekOCRReader)
	assert.True(t, ok, "reader = %T", reader)

	find := func(overrides map[string]string) types.ParserEngineInfo {
		for _, engine := range ListAllEngines(false, overrides, nil) {
			if engine.Name == DeepSeekOCREngineName {
				return engine
			}
		}
		t.Fatalf("%s is not in the engine list", DeepSeekOCREngineName)
		return types.ParserEngineInfo{}
	}
	without := find(nil)
	assert.False(t, without.Available)
	assert.Contains(t, without.UnavailableReason, "API Key")
	for _, fileType := range []string{"pdf", "doc", "docx", "ppt", "pptx", "jpg", "png", "webp"} {
		assert.Contains(t, without.FileTypes, fileType)
	}
	assert.True(t, find(map[string]string{"deepseek_ocr_api_key": "key"}).Available)
}
