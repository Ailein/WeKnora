package im

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// recordingDocReader stands in for the docreader client and records what it
// was asked to read.
type recordingDocReader struct {
	mu       sync.Mutex
	requests []types.ReadRequest
	text     string
}

func (r *recordingDocReader) Read(_ context.Context, req *types.ReadRequest) (*types.ReadResult, error) {
	r.mu.Lock()
	r.requests = append(r.requests, *req)
	r.mu.Unlock()
	text := r.text
	if text == "" {
		text = "docreader text"
	}
	return &types.ReadResult{MarkdownContent: text}, nil
}

func (r *recordingDocReader) Reconnect(string) error { return nil }
func (r *recordingDocReader) IsConnected() bool      { return true }
func (r *recordingDocReader) ListEngines(context.Context, map[string]string) ([]types.ParserEngineInfo, error) {
	return nil, nil
}

func (r *recordingDocReader) PreprocessImage(_ context.Context, image []byte) ([]byte, bool, error) {
	return image, true, nil
}

func (r *recordingDocReader) calls() []types.ReadRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]types.ReadRequest(nil), r.requests...)
}

type readFunc func(context.Context, *types.ReadRequest) (*types.ReadResult, error)

func (f readFunc) Read(ctx context.Context, req *types.ReadRequest) (*types.ReadResult, error) {
	return f(ctx, req)
}

// imTestEngine is a parser engine registered for one test.
type imTestEngine struct {
	name       string
	read       readFunc
	newErr     error
	lastReq    atomic.Pointer[types.ReadRequest]
	readsCount atomic.Int32
}

func (e *imTestEngine) Name() string            { return e.name }
func (e *imTestEngine) Description() string     { return "IM test engine" }
func (e *imTestEngine) FileTypes(bool) []string { return []string{"pdf", "docx", "jpg", "jpeg", "png"} }
func (e *imTestEngine) CheckAvailable(bool, map[string]string) (bool, string) {
	return true, ""
}

func (e *imTestEngine) NewReader(context.Context, docparser.ReaderDeps) (interfaces.DocReader, error) {
	if e.newErr != nil {
		return nil, e.newErr
	}
	return readFunc(func(ctx context.Context, req *types.ReadRequest) (*types.ReadResult, error) {
		copied := *req
		e.lastReq.Store(&copied)
		e.readsCount.Add(1)
		return e.read(ctx, req)
	}), nil
}

// registerIMTestEngine registers an engine under a name unique to the test;
// the registry has no way to remove one, and names must not collide.
func registerIMTestEngine(t *testing.T, read readFunc) *imTestEngine {
	t.Helper()
	engine := &imTestEngine{name: "im_test_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")), read: read}
	docparser.RegisterEngine(engine)
	return engine
}

func textResult(text string) readFunc {
	return func(context.Context, *types.ReadRequest) (*types.ReadResult, error) {
		return &types.ReadResult{MarkdownContent: text}, nil
	}
}

func agentWithRules(rules ...types.ParserEngineRule) *types.CustomAgent {
	return &types.CustomAgent{Config: types.CustomAgentConfig{ChatParserEngineRules: rules}}
}

func rule(engine string, fileTypes ...string) types.ParserEngineRule {
	return types.ParserEngineRule{FileTypes: fileTypes, Engine: engine}
}

var testJPEG = []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}

func prepareTestAttachment(
	t *testing.T, s *Service, messageType MessageType, fileName string, content []byte,
	agent *types.CustomAgent, tenant *types.Tenant,
) (types.MessageAttachment, []string) {
	t.Helper()
	adapter := &attachmentTestAdapter{lifecycleTestAdapter: &lifecycleTestAdapter{}, content: content, fileName: fileName}
	attachments, imageURLs, _, err := s.prepareIMAttachments(context.Background(), &IncomingMessage{
		MessageType: messageType,
		FileName:    fileName,
	}, adapter, agent, tenant)
	if err != nil {
		t.Fatalf("prepareIMAttachments() error = %v", err)
	}
	if len(attachments) != 1 {
		t.Fatalf("attachment count = %d, want 1", len(attachments))
	}
	return attachments[0], imageURLs
}

func TestPrepareIMAttachmentsUsesAgentParserRule(t *testing.T) {
	engine := registerIMTestEngine(t, textResult("ROLE: Usher RM100/day"))
	docreader := &recordingDocReader{}
	tenant := &types.Tenant{ParserEngineConfig: &types.ParserEngineConfig{MinerUAPIKey: "tenant-key"}}

	attachment, imageURLs := prepareTestAttachment(t, &Service{documentReader: docreader}, MessageTypeFile,
		"brief.pdf", []byte("%PDF-1.4 scanned"), agentWithRules(rule(engine.name, "pdf")), tenant)

	if attachment.Content != "ROLE: Usher RM100/day" {
		t.Fatalf("content = %q, want the engine's text", attachment.Content)
	}
	if len(imageURLs) != 0 {
		t.Fatalf("image URLs = %v, want none for a PDF", imageURLs)
	}
	if calls := docreader.calls(); len(calls) != 0 {
		t.Fatalf("docreader was called %d times, want 0", len(calls))
	}
	req := engine.lastReq.Load()
	if req == nil {
		t.Fatal("engine was not called")
	}
	if req.ParserEngine != engine.name || req.FileType != "pdf" || req.FileName != "brief.pdf" {
		t.Fatalf("engine request = engine %q type %q name %q", req.ParserEngine, req.FileType, req.FileName)
	}
	if req.ParserEngineOverrides["mineru_api_key"] != "tenant-key" {
		t.Fatalf("engine request does not carry the workspace parser settings: %v", req.ParserEngineOverrides)
	}
}

func TestPrepareIMAttachmentsKeepsImageForVisionWithParserRule(t *testing.T) {
	engine := registerIMTestEngine(t, textResult("EARN RM 130/DAY"))
	docreader := &recordingDocReader{}

	attachment, imageURLs := prepareTestAttachment(t, &Service{documentReader: docreader}, MessageTypeImage,
		"photo.jpg", testJPEG, agentWithRules(rule(engine.name, "jpg", "jpeg", "png")), nil)

	if attachment.Content != "EARN RM 130/DAY" {
		t.Fatalf("content = %q, want the engine's text", attachment.Content)
	}
	if len(imageURLs) != 1 || !strings.HasPrefix(imageURLs[0], "data:image/jpeg;base64,") {
		t.Fatalf("image URLs = %v, want the photo as a JPEG data URI", imageURLs)
	}
	if calls := docreader.calls(); len(calls) != 0 {
		t.Fatalf("docreader was called %d times, want 0", len(calls))
	}
}

func TestPrepareIMAttachmentsFallsBackWhenTheParserFails(t *testing.T) {
	cases := map[string]readFunc{
		"error": func(context.Context, *types.ReadRequest) (*types.ReadResult, error) {
			return nil, errors.New("upstream 503")
		},
		"error in result": func(context.Context, *types.ReadRequest) (*types.ReadResult, error) {
			// The cloud readers report a missing token this way, with no Go error.
			return &types.ReadResult{Error: "PaddleOCR-VL Cloud token is not configured"}, nil
		},
		"no result": func(context.Context, *types.ReadRequest) (*types.ReadResult, error) {
			return nil, nil
		},
		"no text": textResult("  \n "),
	}
	for name, read := range cases {
		t.Run(name, func(t *testing.T) {
			engine := registerIMTestEngine(t, read)
			docreader := &recordingDocReader{}

			attachment, _ := prepareTestAttachment(t, &Service{documentReader: docreader}, MessageTypeFile,
				"brief.pdf", []byte("%PDF-1.4"), agentWithRules(rule(engine.name, "pdf")), nil)

			if engine.readsCount.Load() != 1 {
				t.Fatalf("engine reads = %d, want 1", engine.readsCount.Load())
			}
			if attachment.Content != "docreader text" {
				t.Fatalf("content = %q, want the default reader's text", attachment.Content)
			}
			calls := docreader.calls()
			if len(calls) != 1 || calls[0].ParserEngine != "" || calls[0].ParserEngineOverrides != nil {
				t.Fatalf("docreader calls = %+v, want one with no engine named, as before", calls)
			}
		})
	}
}

func TestPrepareIMAttachmentsFallsBackWhenTheParserCannotBeBuilt(t *testing.T) {
	engine := registerIMTestEngine(t, textResult("unused"))
	engine.newErr = errors.New("credentials missing")
	docreader := &recordingDocReader{}

	attachment, _ := prepareTestAttachment(t, &Service{documentReader: docreader}, MessageTypeFile,
		"brief.pdf", []byte("%PDF-1.4"), agentWithRules(rule(engine.name, "pdf")), nil)

	if attachment.Content != "docreader text" {
		t.Fatalf("content = %q, want the default reader's text", attachment.Content)
	}
}

func TestPrepareIMAttachmentsDoesNotWaitPastTheAgentsParseWait(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	// This engine ignores its context, as the docreader client does.
	engine := registerIMTestEngine(t, func(context.Context, *types.ReadRequest) (*types.ReadResult, error) {
		<-release
		return &types.ReadResult{MarkdownContent: "too late"}, nil
	})
	agent := agentWithRules(rule(engine.name, "pdf"))
	agent.Config.AttachmentParseWaitTimeoutSec = 1
	docreader := &recordingDocReader{}

	started := time.Now()
	attachment, _ := prepareTestAttachment(t, &Service{documentReader: docreader}, MessageTypeFile,
		"brief.pdf", []byte("%PDF-1.4"), agent, nil)
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("prepareIMAttachments took %s, want about the 1 s parse wait", took)
	}
	if attachment.Content != "docreader text" {
		t.Fatalf("content = %q, want the default reader's text", attachment.Content)
	}
}

func TestPrepareIMAttachmentsUsesTheWorkspaceRuleWhenTheAgentHasNone(t *testing.T) {
	engine := registerIMTestEngine(t, textResult("workspace engine text"))
	tenant := &types.Tenant{ParserEngineConfig: &types.ParserEngineConfig{
		ChatParserEngineRules: []types.ParserEngineRule{rule(engine.name, "pdf")},
	}}

	attachment, _ := prepareTestAttachment(t, &Service{documentReader: &recordingDocReader{}}, MessageTypeFile,
		"brief.pdf", []byte("%PDF-1.4"), agentWithRules(), tenant)

	if attachment.Content != "workspace engine text" {
		t.Fatalf("content = %q, want the workspace engine's text", attachment.Content)
	}
}

func TestPrepareIMAttachmentsPrefersTheAgentRuleToTheWorkspaceRule(t *testing.T) {
	agentEngine := registerIMTestEngine(t, textResult("agent engine text"))
	workspaceEngine := &imTestEngine{name: agentEngine.name + "_workspace", read: textResult("workspace engine text")}
	docparser.RegisterEngine(workspaceEngine)
	tenant := &types.Tenant{ParserEngineConfig: &types.ParserEngineConfig{
		ChatParserEngineRules: []types.ParserEngineRule{rule(workspaceEngine.name, "pdf")},
	}}

	attachment, _ := prepareTestAttachment(t, &Service{documentReader: &recordingDocReader{}}, MessageTypeFile,
		"brief.pdf", []byte("%PDF-1.4"), agentWithRules(rule(agentEngine.name, "pdf")), tenant)

	if attachment.Content != "agent engine text" {
		t.Fatalf("content = %q, want the agent engine's text", attachment.Content)
	}
	if workspaceEngine.readsCount.Load() != 0 {
		t.Fatal("the workspace engine was used although the agent has a rule")
	}
}

func TestPrepareIMAttachmentsWithoutRulesReadsAsBefore(t *testing.T) {
	docreader := &recordingDocReader{text: "scanned page placeholder"}

	attachment, _ := prepareTestAttachment(t, &Service{documentReader: docreader}, MessageTypeFile,
		"brief.pdf", []byte("%PDF-1.4"), agentWithRules(), &types.Tenant{})

	if attachment.Content != "scanned page placeholder" {
		t.Fatalf("content = %q, want the docreader text", attachment.Content)
	}
	calls := docreader.calls()
	if len(calls) != 1 || calls[0].ParserEngine != "" || calls[0].ParserEngineOverrides != nil {
		t.Fatalf("docreader calls = %+v, want one with no engine named", calls)
	}
}

func TestPrepareIMAttachmentsBuiltinRuleReadsOnceWithDocreader(t *testing.T) {
	docreader := &recordingDocReader{}

	attachment, _ := prepareTestAttachment(t, &Service{documentReader: docreader}, MessageTypeFile,
		"brief.pdf", []byte("%PDF-1.4"), agentWithRules(rule(docparser.BuiltinEngineName, "pdf")), nil)

	if attachment.Content != "docreader text" {
		t.Fatalf("content = %q, want the docreader text", attachment.Content)
	}
	calls := docreader.calls()
	if len(calls) != 1 || calls[0].ParserEngine != docparser.BuiltinEngineName {
		t.Fatalf("docreader calls = %+v, want one naming the builtin engine", calls)
	}
}

func TestPrepareIMAttachmentsSimpleRuleOnAnImageSkipsDocreader(t *testing.T) {
	docreader := &recordingDocReader{}

	attachment, imageURLs := prepareTestAttachment(t, &Service{documentReader: docreader}, MessageTypeImage,
		"photo.jpg", testJPEG, agentWithRules(rule(docparser.SimpleEngineName, "jpg", "jpeg")), nil)

	if attachment.Content != "![photo.jpg](images/photo.jpg)" {
		t.Fatalf("content = %q, want the image placeholder", attachment.Content)
	}
	if len(imageURLs) != 1 {
		t.Fatalf("image URLs = %v, want the photo", imageURLs)
	}
	if calls := docreader.calls(); len(calls) != 0 {
		t.Fatalf("docreader was called %d times, want 0", len(calls))
	}
}

func TestIMAttachmentParserEngineMatching(t *testing.T) {
	agent := agentWithRules(
		rule("", "pdf"), // a rule with no engine is skipped
		rule("engine_a", ".PDF", "Docx"),
		rule("engine_b", " JPEG "),
		rule("auto", "png"),
	)
	tenant := &types.Tenant{ParserEngineConfig: &types.ParserEngineConfig{
		ChatParserEngineRules: []types.ParserEngineRule{rule("engine_w", "pdf", "xlsx")},
	}}
	cases := []struct {
		fileType string
		want     string
	}{
		{"pdf", "engine_a"},
		{".pdf", "engine_a"},
		{"DOCX", "engine_a"},
		{"jpeg", "engine_b"},
		{"png", ""},
		{"xlsx", "engine_w"},
		{"pptx", types.DefaultParserEngine("pptx")},
		{"", ""},
	}
	for _, tc := range cases {
		if got := imAttachmentParserEngine(agent, tenant, tc.fileType); got != tc.want {
			t.Errorf("imAttachmentParserEngine(%q) = %q, want %q", tc.fileType, got, tc.want)
		}
	}
	if got := imAttachmentParserEngine(nil, nil, "pdf"); got != types.DefaultParserEngine("pdf") {
		t.Errorf("with no agent or workspace, pdf = %q, want the type default %q", got, types.DefaultParserEngine("pdf"))
	}
}

func TestIMAttachmentParseWait(t *testing.T) {
	t.Setenv("WEKNORA_CHAT_ATTACHMENT_WAIT_TIMEOUT_SEC", "")
	if got := imAttachmentParseWait(nil); got != imDefaultAttachmentParseWait {
		t.Fatalf("default wait = %s, want %s", got, imDefaultAttachmentParseWait)
	}
	t.Setenv("WEKNORA_CHAT_ATTACHMENT_WAIT_TIMEOUT_SEC", "90")
	if got := imAttachmentParseWait(agentWithRules()); got != 90*time.Second {
		t.Fatalf("wait from the environment = %s, want 90s", got)
	}
	agent := agentWithRules()
	agent.Config.AttachmentParseWaitTimeoutSec = 150
	if got := imAttachmentParseWait(agent); got != 150*time.Second {
		t.Fatalf("wait from the agent = %s, want 150s", got)
	}
}
