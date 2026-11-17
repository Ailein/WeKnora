package asr

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/api"
	"github.com/Tencent/WeKnora/internal/models/api/openaichataudio"
	"github.com/Tencent/WeKnora/internal/models/api/openaitranscriptions"
	"github.com/Tencent/WeKnora/internal/models/providers"
	modelruntime "github.com/Tencent/WeKnora/internal/models/runtime"
	"github.com/Tencent/WeKnora/internal/types"
)

// retryPolicy is the transport-error retry budget, which is none. The
// knowledge pipeline retries a failed transcription as a whole task, and a
// client-level retry on top would turn one hung 300-second upload into
// several.
var retryPolicy = func() api.RetryPolicy { return api.RetryPolicy{} }

// newASR resolves the catalog and returns the protocol client for the
// configured model. It mirrors rerank.newReranker and
// embedding.newRemoteEmbedder: the vendor's facts decide the protocol, the
// URL and the credential, and this function knows no vendor names.
func newASR(config *Config) (ASR, error) {
	if config == nil {
		return nil, fmt.Errorf("asr config is nil")
	}
	if strings.TrimSpace(config.ModelName) == "" {
		return nil, fmt.Errorf("model name is required")
	}
	resolved, err := modelruntime.Resolve(modelruntime.Ref{
		Provider:  config.Provider,
		Model:     config.ModelName,
		BaseURL:   config.BaseURL,
		ModelType: types.ModelTypeASR,
		Extra:     config.ExtraConfig,
		Override:  config.Spec,
	})
	if err != nil {
		return nil, err
	}
	// A vendor that does not declare speech recognition has not been checked
	// for it, and its Endpoint hook may still compute a path — Azure's does.
	// A row that named the vendor is refused; a row that named none and was
	// matched by its URL is what the pre-catalog client served: an
	// OpenAI-compatible endpoint at that URL.
	if !resolved.Vendor.SupportsType(types.ModelTypeASR) {
		if strings.TrimSpace(config.Provider) != "" {
			return nil, fmt.Errorf("%s does not offer speech recognition in this build", resolved.Vendor.Name)
		}
		resolved, err = modelruntime.Resolve(modelruntime.Ref{
			Provider:  providers.GenericID,
			Model:     config.ModelName,
			BaseURL:   config.BaseURL,
			ModelType: types.ModelTypeASR,
			Extra:     config.ExtraConfig,
			Override:  config.Spec,
		})
		if err != nil {
			return nil, err
		}
	}
	if err := validateASRBaseURL(resolved.BaseURL); err != nil {
		return nil, err
	}

	vendor := resolved.Vendor
	endpoint, err := resolved.Endpoint(types.ModelTypeASR, modelruntime.Connection{
		ModelID:     config.ModelID,
		Credentials: api.Credentials{APIKey: config.APIKey},
		Headers:     config.CustomHeaders,
		Extra:       config.ExtraConfig,
		Client:      newASRHTTPClient(time.Duration(resolved.Transcriptions.RequestTimeout) * time.Second),
	})
	if err != nil {
		return nil, err
	}
	if endpoint.URL != "" {
		if err := validateASRBaseURL(endpoint.URL); err != nil {
			return nil, err
		}
	}
	settings := resolved.Transcriptions

	var client api.Transcriber
	switch resolved.TranscriptionAPI {
	case api.TranscriptionOpenAI:
		client = openaitranscriptions.New(openaitranscriptions.Config{
			Endpoint: endpoint, Settings: settings, Retry: retryPolicy(),
		})
	case api.TranscriptionChatAudio:
		client = openaichataudio.New(openaichataudio.Config{
			Endpoint: endpoint, Settings: settings, Retry: retryPolicy(),
		})
	default:
		return nil, fmt.Errorf("unsupported transcription api %q for provider %s",
			resolved.TranscriptionAPI, vendor.ID)
	}
	return &protocolASR{
		inner:     client,
		settings:  settings,
		vendor:    vendor.Name,
		endpoint:  resolved.BaseURL,
		modelName: config.ModelName,
		modelID:   config.ModelID,
	}, nil
}

// protocolASR adapts a protocol client to the ASR interface and refuses an
// upload the vendor has documented it will not take, before sending it.
type protocolASR struct {
	inner     api.Transcriber
	settings  api.TranscriptionsSettings
	vendor    string
	endpoint  string
	modelName string
	modelID   string
}

func (a *protocolASR) GetModelName() string { return a.modelName }
func (a *protocolASR) GetModelID() string   { return a.modelID }

func (a *protocolASR) Transcribe(ctx context.Context, audio []byte, fileName string) (*TranscriptionResult, error) {
	if len(audio) == 0 {
		return nil, fmt.Errorf("audio bytes are empty")
	}
	// The server identifies the format from the extension.
	if fileName == "" {
		fileName = "audio.mp3"
	}
	pcm, notConverted := a.prepare(ctx, audio, fileName)
	if pcm != nil {
		return a.transcribePieces(ctx, pcm, fileName)
	}
	why := ""
	if notConverted != nil {
		why = " (" + notConverted.Error() + ")"
	}
	if limit := a.settings.MaxFileBytes; limit > 0 && len(audio) > limit {
		return nil, fmt.Errorf("%s transcription: the audio is %.1f MB; %s accepts at most %d MB per file%s",
			a.modelName, float64(len(audio))/(1<<20), a.vendor, limit>>20, why)
	}
	if formats := a.settings.Formats; len(formats) > 0 {
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(fileName)), ".")
		if !slices.Contains(formats, ext) {
			return nil, fmt.Errorf("%s transcription: %s accepts %s audio, not %q%s",
				a.modelName, a.vendor, strings.Join(formats, "/"), fileName, why)
		}
	}
	return a.send(ctx, audio, fileName)
}

// prepare returns the audio as 16 kHz mono PCM when it must be converted
// before the vendor takes it: a format it does not list, a file over its size
// cap, a recording over its duration limit. Otherwise it returns nil, and the
// file goes as it is or, when it needed converting and could not be (no
// ffmpeg, a container this build does not open, a file ffmpeg cannot read),
// Transcribe refuses it with the reason prepare gives.
func (a *protocolASR) prepare(ctx context.Context, audio []byte, fileName string) ([]byte, error) {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(fileName)), ".")
	formatOK := len(a.settings.Formats) == 0 || slices.Contains(a.settings.Formats, ext)
	sizeOK := (a.settings.MaxFileBytes <= 0 || len(audio) <= a.settings.MaxFileBytes) &&
		(a.settings.MaxEncodedBytes <= 0 ||
			dataURIPrefixBytes+base64.StdEncoding.EncodedLen(len(audio)) <= a.settings.MaxEncodedBytes)
	maxDuration := float64(a.settings.MaxDurationSeconds)
	if formatOK && sizeOK && maxDuration <= 0 {
		return nil, nil
	}
	if len(a.settings.Formats) > 0 && !slices.Contains(a.settings.Formats, "wav") {
		return nil, fmt.Errorf("converted audio is WAV, which %s does not take", a.vendor)
	}
	pcm, complete, err := decodePCM(ctx, audio, ext)
	if err == nil && !complete {
		err = fmt.Errorf("the audio is longer than %s, the most this build converts",
			time.Duration(maxPreparedSeconds)*time.Second)
	}
	if err != nil {
		if formatOK && sizeOK && !errors.Is(err, errNoConverter) {
			// Only the length was in doubt, and the vendor judges that.
			logger.Warnf(ctx, "[ASR] %s: sending %q as it is: %v", a.modelName, fileName, err)
		}
		return nil, err
	}
	seconds := float64(len(pcm)) / prepBytesPerSecond
	if formatOK && sizeOK && (maxDuration <= 0 || seconds <= maxDuration-pieceMarginSeconds) {
		return nil, nil
	}
	return pcm, nil
}

// maxPieceSeconds is the longest WAV piece the vendor takes: inside its
// duration limit by pieceMarginSeconds and within its size caps; 0 when it
// states none.
func (a *protocolASR) maxPieceSeconds() float64 {
	var limit float64
	tighten := func(seconds float64) {
		if seconds > 0 && (limit == 0 || seconds < limit) {
			limit = seconds
		}
	}
	if d := a.settings.MaxDurationSeconds; d > 0 {
		tighten(float64(d) - pieceMarginSeconds)
	}
	if b := a.settings.MaxFileBytes; b > 0 {
		tighten(float64(b-wavHeaderBytes) / prepBytesPerSecond)
	}
	if b := a.settings.MaxEncodedBytes; b > 0 {
		tighten(float64((b-dataURIPrefixBytes)/4*3-wavHeaderBytes) / prepBytesPerSecond)
	}
	return limit
}

// transcribePieces sends pcm as WAV, in as many pieces as the vendor's limits
// need, and joins what comes back. One failed piece fails the whole: half a
// message read back as all of it would mislead.
func (a *protocolASR) transcribePieces(ctx context.Context, pcm []byte, fileName string) (*TranscriptionResult, error) {
	seconds := float64(len(pcm)) / prepBytesPerSecond
	bounds := audible(pcm, pieceBounds(pcm, a.maxPieceSeconds()))
	stem := strings.TrimSuffix(filepath.Base(fileName), filepath.Ext(fileName))
	if stem == "" || stem == "." {
		stem = "audio"
	}
	logger.Infof(ctx, "[ASR] %s: %s (%.1f s) converted to WAV, %d piece(s)", a.modelName, fileName, seconds, len(bounds))
	result := &TranscriptionResult{Duration: seconds}
	texts := make([]string, 0, len(bounds))
	for i, b := range bounds {
		name := stem + ".wav"
		if len(bounds) > 1 {
			name = fmt.Sprintf("%s-%d.wav", stem, i+1)
		}
		out, err := a.send(ctx, wavFile(pcm[b[0]:b[1]]), name)
		if err != nil {
			if len(bounds) > 1 {
				return nil, fmt.Errorf("piece %d of %d: %w", i+1, len(bounds), err)
			}
			return nil, err
		}
		texts = append(texts, out.Text)
		offset := float64(b[0]) / prepBytesPerSecond
		for _, s := range out.Segments {
			result.Segments = append(result.Segments, Segment{Start: s.Start + offset, End: s.End + offset, Text: s.Text})
		}
	}
	result.Text = joinTranscripts(texts)
	return result, nil
}

// send makes one request.
func (a *protocolASR) send(ctx context.Context, audio []byte, fileName string) (*TranscriptionResult, error) {
	logger.Infof(ctx, "[ASR] transcribing model=%s endpoint=%s size=%d file=%s",
		a.modelName, a.endpoint, len(audio), fileName)

	out, err := a.inner.Transcribe(ctx, api.TranscriptionRequest{
		Audio: audio, FileName: fileName, Language: languageFrom(ctx),
	})
	if err != nil {
		return nil, fmt.Errorf("ASR transcription request failed: %w", err)
	}
	result := &TranscriptionResult{Text: out.Text, Duration: out.Duration}
	for _, s := range out.Segments {
		result.Segments = append(result.Segments, Segment{Start: s.Start, End: s.End, Text: s.Text})
	}
	logger.Infof(ctx, "[ASR] transcription completed, text length=%d", len(result.Text))
	return result, nil
}
