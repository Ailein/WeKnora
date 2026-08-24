package im

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/asr"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// voiceTestAdapter serves canned audio bytes through the FileDownloader path.
type voiceTestAdapter struct {
	lifecycleTestAdapter
	audio []byte
	err   error
}

func (a *voiceTestAdapter) DownloadFile(context.Context, *IncomingMessage) (io.ReadCloser, string, error) {
	if a.err != nil {
		return nil, "", a.err
	}
	return io.NopCloser(strings.NewReader(string(a.audio))), "voice.ogg", nil
}

// fakeASR returns a fixed transcript; the embedded interface makes only
// Transcribe/GetModelName/GetModelID available, which is all voice uses.
type fakeASR struct {
	text string
	err  error
}

func (f *fakeASR) Transcribe(context.Context, []byte, string) (*asr.TranscriptionResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &asr.TranscriptionResult{Text: f.text}, nil
}
func (f *fakeASR) GetModelName() string { return "fake-asr" }
func (f *fakeASR) GetModelID() string   { return "asr-1" }

// voiceModelService fakes just GetASRModel; other MessageService methods panic
// via the embedded nil, same pattern as manualReplyMessageService.
type voiceModelService struct {
	interfaces.ModelService
	model asr.ASR
	err   error
}

func (f *voiceModelService) GetASRModel(context.Context, string) (asr.ASR, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.model, nil
}

func voiceAgent() *types.CustomAgent {
	return &types.CustomAgent{Config: types.CustomAgentConfig{
		AudioUploadEnabled: true,
		ASRModelID:         "asr-1",
	}}
}

func voiceMsg() *IncomingMessage {
	return &IncomingMessage{
		Platform:    PlatformWhatsApp,
		MessageType: MessageTypeVoice,
		UserID:      "8613800138000",
		FileKey:     "media-key",
		FileName:    "voice.ogg",
		FileSize:    2048,
	}
}

func TestTranscribeIMVoiceSuccess(t *testing.T) {
	svc := &Service{modelService: &voiceModelService{model: &fakeASR{text: "  你们几点营业？  "}}}
	adapter := &voiceTestAdapter{audio: []byte("opus-bytes")}

	got, err := svc.transcribeIMVoice(context.Background(), voiceMsg(), adapter, voiceAgent())
	if err != nil {
		t.Fatalf("transcribeIMVoice: %v", err)
	}
	if got != "你们几点营业？" {
		t.Fatalf("transcript = %q, want trimmed text", got)
	}
}

func TestTranscribeIMVoiceTruncatesLongTranscript(t *testing.T) {
	long := strings.Repeat("啊", maxContentLength+100)
	svc := &Service{modelService: &voiceModelService{model: &fakeASR{text: long}}}
	adapter := &voiceTestAdapter{audio: []byte("opus-bytes")}

	got, err := svc.transcribeIMVoice(context.Background(), voiceMsg(), adapter, voiceAgent())
	if err != nil {
		t.Fatalf("transcribeIMVoice: %v", err)
	}
	if runes := []rune(got); len(runes) != maxContentLength {
		t.Fatalf("transcript length = %d runes, want capped at %d", len(runes), maxContentLength)
	}
}

func TestTranscribeIMVoiceNotEnabled(t *testing.T) {
	svc := &Service{modelService: &voiceModelService{model: &fakeASR{text: "hi"}}}
	adapter := &voiceTestAdapter{audio: []byte("opus-bytes")}

	cases := []struct {
		name  string
		agent *types.CustomAgent
	}{
		{"nil agent", nil},
		{"audio upload disabled", &types.CustomAgent{Config: types.CustomAgentConfig{ASRModelID: "asr-1"}}},
		{"no asr model", &types.CustomAgent{Config: types.CustomAgentConfig{AudioUploadEnabled: true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.transcribeIMVoice(context.Background(), voiceMsg(), adapter, tc.agent); !errors.Is(err, errVoiceNotEnabled) {
				t.Fatalf("err = %v, want errVoiceNotEnabled", err)
			}
		})
	}
}

func TestTranscribeIMVoiceSizeLimits(t *testing.T) {
	svc := &Service{modelService: &voiceModelService{model: &fakeASR{text: "hi"}}}

	// Declared size over the cap is rejected before any download.
	huge := voiceMsg()
	huge.FileSize = maxIMVoiceBytes + 1
	if _, err := svc.transcribeIMVoice(context.Background(), huge, &voiceTestAdapter{}, voiceAgent()); !errors.Is(err, errVoiceTooLarge) {
		t.Fatalf("declared-size err = %v, want errVoiceTooLarge", err)
	}

	// The declared size is sender-controlled: an under-reported payload must
	// still be capped on the actual stream.
	lying := voiceMsg()
	lying.FileSize = 10
	adapter := &voiceTestAdapter{audio: make([]byte, maxIMVoiceBytes+1)}
	if _, err := svc.transcribeIMVoice(context.Background(), lying, adapter, voiceAgent()); !errors.Is(err, errVoiceTooLarge) {
		t.Fatalf("stream-size err = %v, want errVoiceTooLarge", err)
	}
}

func TestTranscribeIMVoiceFailures(t *testing.T) {
	agent := voiceAgent()
	msg := voiceMsg()

	// Adapter without FileDownloader support.
	svc := &Service{modelService: &voiceModelService{model: &fakeASR{text: "hi"}}}
	if _, err := svc.transcribeIMVoice(context.Background(), msg, &lifecycleTestAdapter{}, agent); err == nil {
		t.Fatal("adapter without FileDownloader must fail")
	}

	// Download failure.
	if _, err := svc.transcribeIMVoice(context.Background(), msg, &voiceTestAdapter{err: errors.New("expired")}, agent); err == nil {
		t.Fatal("download failure must propagate")
	}

	// ASR model resolution failure.
	svc = &Service{modelService: &voiceModelService{err: errors.New("no such model")}}
	if _, err := svc.transcribeIMVoice(context.Background(), msg, &voiceTestAdapter{audio: []byte("x")}, agent); err == nil {
		t.Fatal("model resolution failure must propagate")
	}

	// Transcription failure.
	svc = &Service{modelService: &voiceModelService{model: &fakeASR{err: errors.New("provider 500")}}}
	if _, err := svc.transcribeIMVoice(context.Background(), msg, &voiceTestAdapter{audio: []byte("x")}, agent); err == nil {
		t.Fatal("transcription failure must propagate")
	}

	// Empty transcript (silence / noise).
	svc = &Service{modelService: &voiceModelService{model: &fakeASR{text: "   "}}}
	if _, err := svc.transcribeIMVoice(context.Background(), msg, &voiceTestAdapter{audio: []byte("x")}, agent); !errors.Is(err, errVoiceEmpty) {
		t.Fatal("blank transcript must map to errVoiceEmpty")
	}
}

func TestVoiceErrorReply(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errVoiceNotEnabled, "未开启语音识别"},
		{errVoiceTooLarge, "语音太大"},
		{errVoiceEmpty, "没有听清"},
		{errors.New("anything else"), "语音识别失败"},
	}
	for _, tc := range cases {
		if got := voiceErrorReply(tc.err); !strings.Contains(got, tc.want) {
			t.Errorf("voiceErrorReply(%v) = %q, want mention of %q", tc.err, got, tc.want)
		}
	}
}
