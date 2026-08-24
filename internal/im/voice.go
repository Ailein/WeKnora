package im

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	// maxIMVoiceBytes bounds a voice message buffered for transcription. ASR
	// providers commonly cap uploads around 25 MB; staying under that keeps a
	// clear local error instead of an opaque provider one.
	maxIMVoiceBytes = 16 << 20 // 16 MiB
	// imVoiceTranscribeTimeout bounds the ASR call itself, on top of the
	// download timeout, so a hung provider cannot pin a QA worker.
	imVoiceTranscribeTimeout = 2 * time.Minute
)

// Sentinel errors pick the user-facing reply in voiceErrorReply.
var (
	errVoiceNotEnabled = errors.New("voice transcription is not enabled for this agent")
	errVoiceTooLarge   = fmt.Errorf("voice message exceeds the %d MiB limit", maxIMVoiceBytes>>20)
	errVoiceEmpty      = errors.New("transcription returned no text")
)

// voiceErrorReply maps a transcription failure to the reply sent to the IM
// user. Hardcoded Chinese, consistent with the other pipeline replies.
func voiceErrorReply(err error) string {
	switch {
	case errors.Is(err, errVoiceNotEnabled):
		return "当前渠道暂未开启语音识别，请发送文字消息。"
	case errors.Is(err, errVoiceTooLarge):
		return fmt.Sprintf("这条语音太大了（超过 %d MB），请发送较短的语音或改用文字。", maxIMVoiceBytes>>20)
	case errors.Is(err, errVoiceEmpty):
		return "没有听清这条语音的内容，请重新发送或改用文字描述。"
	default:
		return "语音识别失败，请稍后重试或改用文字描述。"
	}
}

// transcribeIMVoice downloads a voice message through the adapter and runs it
// through the agent's ASR model, returning the transcript that will stand in
// for the message text. It reuses the web attachment pipeline's agent-level
// switch (audio_upload_enabled + asr_model_id), so enabling voice input for an
// agent covers both the web console and its IM channels.
func (s *Service) transcribeIMVoice(
	ctx context.Context, msg *IncomingMessage, adapter Adapter, agent *types.CustomAgent,
) (string, error) {
	if agent == nil || !agent.Config.AudioUploadEnabled || agent.Config.ASRModelID == "" || s.modelService == nil {
		return "", errVoiceNotEnabled
	}
	if msg.FileSize > maxIMVoiceBytes {
		return "", errVoiceTooLarge
	}
	downloader, ok := adapter.(FileDownloader)
	if !ok {
		return "", fmt.Errorf("platform %s does not support voice download", msg.Platform)
	}

	downloadCtx, cancel := context.WithTimeout(ctx, imAttachmentReadTimeout)
	defer cancel()
	reader, fileName, err := downloader.DownloadFile(downloadCtx, msg)
	if err != nil {
		return "", fmt.Errorf("download voice message: %w", err)
	}
	defer reader.Close()
	// The declared FileSize comes from the sender; enforce the cap on the stream.
	audio, err := io.ReadAll(io.LimitReader(reader, maxIMVoiceBytes+1))
	if err != nil {
		return "", fmt.Errorf("read voice message: %w", err)
	}
	if len(audio) > maxIMVoiceBytes {
		return "", errVoiceTooLarge
	}
	if fileName == "" {
		fileName = msg.FileName
	}

	asrModel, err := s.modelService.GetASRModel(ctx, agent.Config.ASRModelID)
	if err != nil {
		return "", fmt.Errorf("get ASR model %s: %w", agent.Config.ASRModelID, err)
	}
	transcribeCtx, cancelTranscribe := context.WithTimeout(ctx, imVoiceTranscribeTimeout)
	defer cancelTranscribe()
	result, err := asrModel.Transcribe(transcribeCtx, audio, fileName)
	if err != nil {
		return "", fmt.Errorf("transcribe voice message: %w", err)
	}
	transcript := strings.TrimSpace(result.Text)
	if transcript == "" {
		return "", errVoiceEmpty
	}
	if runes := []rune(transcript); len(runes) > maxContentLength {
		transcript = string(runes[:maxContentLength])
	}
	logger.Infof(ctx, "[IM] Voice transcribed: platform=%s user=%s bytes=%d transcript_len=%d",
		msg.Platform, msg.UserID, len(audio), len(transcript))
	return transcript, nil
}
