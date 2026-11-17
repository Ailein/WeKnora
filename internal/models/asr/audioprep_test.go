package asr

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/api"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTranscriber records each request and answers it with answer, or with
// "part1", "part2", ... when answer is nil.
type fakeTranscriber struct {
	requests []api.TranscriptionRequest
	answer   func(i int, req api.TranscriptionRequest) (*api.Transcription, error)
}

func (f *fakeTranscriber) Transcribe(_ context.Context, req api.TranscriptionRequest) (*api.Transcription, error) {
	f.requests = append(f.requests, req)
	if f.answer != nil {
		return f.answer(len(f.requests)-1, req)
	}
	return &api.Transcription{Text: fmt.Sprintf("part%d", len(f.requests))}, nil
}

// zhipuLike has Zhipu's documented limits: wav or mp3, 25 MB, 30 seconds.
func zhipuLike(inner api.Transcriber) *protocolASR {
	return &protocolASR{
		inner: inner, vendor: "Zhipu BigModel", modelName: "glm-asr-2512",
		settings: api.TranscriptionsSettings{Formats: []string{"wav", "mp3"}, MaxFileBytes: 25 << 20, MaxDurationSeconds: 30},
	}
}

// pcmPattern is 16 kHz mono PCM, total seconds long: a 440 Hz tone for on
// seconds, then off seconds of silence, over and over (off 0: tone throughout).
func pcmPattern(total, on, off float64) []byte {
	n := int(total * prepSampleRate)
	pcm := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		t := float64(i) / prepSampleRate
		if off == 0 || math.Mod(t, on+off) < on {
			binary.LittleEndian.PutUint16(pcm[2*i:], uint16(int16(8000*math.Sin(2*math.Pi*440*t))))
		}
	}
	return pcm
}

func seconds(bytes int) float64 { return float64(bytes) / prepBytesPerSecond }

func needFFmpeg(t *testing.T) string {
	t.Helper()
	bin, err := ffmpegPath()
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	return bin
}

func withoutFFmpeg(t *testing.T) {
	t.Helper()
	saved := ffmpegPath
	ffmpegPath = func() (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { ffmpegPath = saved })
}

// encode turns PCM into a file of the given extension with ffmpeg.
func encode(t *testing.T, pcm []byte, ext string, args ...string) []byte {
	t.Helper()
	bin := needFFmpeg(t)
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.wav"), filepath.Join(dir, "out."+ext)
	require.NoError(t, os.WriteFile(in, wavFile(pcm), 0o600))
	argv := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-i", in}, args...)
	msg, err := exec.Command(bin, append(argv, out)...).CombinedOutput()
	require.NoError(t, err, string(msg))
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	return data
}

// wavSeconds checks the header wavFile writes and returns the length.
func wavSeconds(t *testing.T, wav []byte) float64 {
	t.Helper()
	require.GreaterOrEqual(t, len(wav), wavHeaderBytes)
	require.Equal(t, "RIFF", string(wav[0:4]))
	require.Equal(t, "WAVEfmt ", string(wav[8:16]))
	require.EqualValues(t, 1, binary.LittleEndian.Uint16(wav[20:]), "PCM")
	require.EqualValues(t, 1, binary.LittleEndian.Uint16(wav[22:]), "mono")
	require.EqualValues(t, prepSampleRate, binary.LittleEndian.Uint32(wav[24:]))
	require.EqualValues(t, 16, binary.LittleEndian.Uint16(wav[34:]))
	require.Equal(t, "data", string(wav[36:40]))
	require.EqualValues(t, len(wav)-wavHeaderBytes, binary.LittleEndian.Uint32(wav[40:]))
	require.EqualValues(t, len(wav)-8, binary.LittleEndian.Uint32(wav[4:]))
	return seconds(len(wav) - wavHeaderBytes)
}

func TestWavFileWrapsThePCM(t *testing.T) {
	pcm := pcmPattern(1.5, 1, 0)
	wav := wavFile(pcm)
	assert.InDelta(t, 1.5, wavSeconds(t, wav), 1e-9)
	assert.Equal(t, pcm, wav[wavHeaderBytes:])
}

func TestPiecesAreCutInThePauseBeforeTheLimit(t *testing.T) {
	// 5 s of tone, 2 s of pause: pauses at 5-7, 12-14, 19-21, 26-28, ...
	pcm := pcmPattern(65, 5, 2)
	bounds := pieceBounds(pcm, 29)
	require.Len(t, bounds, 3)
	assert.Equal(t, 0, bounds[0][0])
	assert.Equal(t, len(pcm), bounds[2][1])
	for i, b := range bounds {
		if i > 0 {
			assert.Equal(t, bounds[i-1][1], b[0], "pieces follow on")
		}
		assert.LessOrEqual(t, seconds(b[1]-b[0]), 29.0)
	}
	assert.True(t, seconds(bounds[0][1]) > 26 && seconds(bounds[0][1]) < 28, "first cut at %.2f s", seconds(bounds[0][1]))
	assert.True(t, seconds(bounds[1][1]) > 54 && seconds(bounds[1][1]) < 56, "second cut at %.2f s", seconds(bounds[1][1]))
}

func TestPiecesWithoutAPauseStayUnderTheLimit(t *testing.T) {
	pcm := pcmPattern(70, 1, 0)
	bounds := pieceBounds(pcm, 29)
	require.Len(t, bounds, 3)
	for i, b := range bounds {
		d := seconds(b[1] - b[0])
		assert.LessOrEqual(t, d, 29.0)
		if i < len(bounds)-1 {
			assert.GreaterOrEqual(t, d, 21.0, "a cut is looked for only in the last 8 seconds")
		}
	}
}

func TestAShortRecordingIsOnePiece(t *testing.T) {
	pcm := pcmPattern(10, 1, 0)
	assert.Equal(t, [][2]int{{0, len(pcm)}}, pieceBounds(pcm, 29))
	assert.Equal(t, [][2]int{{0, len(pcm)}}, pieceBounds(pcm, 0), "no limit")
}

func TestSilentPiecesAreNotSentUnlessAllAre(t *testing.T) {
	second := prepBytesPerSecond
	pcm := append(pcmPattern(3, 3, 0), make([]byte, 3*second)...)
	bounds := [][2]int{{0, 3 * second}, {3 * second, 6 * second}}
	assert.Equal(t, bounds[:1], audible(pcm, bounds))

	quiet := make([]byte, 4*second)
	all := [][2]int{{0, 2 * second}, {2 * second, 4 * second}}
	assert.Equal(t, all, audible(quiet, all))
}

func TestJoinTranscripts(t *testing.T) {
	assert.Equal(t, "你好，我们在吉隆坡招人。", joinTranscripts([]string{"你好，", " 我们在吉隆坡招人。 "}))
	assert.Equal(t, "Hello there general Kenobi", joinTranscripts([]string{"Hello there", "general Kenobi"}))
	assert.Equal(t, "日薪RM120", joinTranscripts([]string{"日薪", "RM120"}))
	assert.Equal(t, "Gaji sehari seratus ringgit", joinTranscripts([]string{"Gaji sehari", "seratus ringgit"}))
	assert.Equal(t, "only", joinTranscripts([]string{"", " only ", ""}))
	assert.Equal(t, "", joinTranscripts(nil))
}

func TestMaxPieceSeconds(t *testing.T) {
	cases := []struct {
		name     string
		settings api.TranscriptionsSettings
		want     float64
	}{
		{"30 seconds and 25 MB", api.TranscriptionsSettings{MaxDurationSeconds: 30, MaxFileBytes: 25 << 20}, 29},
		{"500 seconds and 50 MB", api.TranscriptionsSettings{MaxDurationSeconds: 500, MaxFileBytes: 50 << 20}, 499},
		{"25 MB of WAV", api.TranscriptionsSettings{MaxFileBytes: 25 << 20}, seconds(25<<20 - wavHeaderBytes)},
		{"a 10 MB data URI", api.TranscriptionsSettings{MaxEncodedBytes: 10 << 20},
			seconds((10<<20-dataURIPrefixBytes)/4*3 - wavHeaderBytes)},
		{"no limit", api.TranscriptionsSettings{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.InDelta(t, c.want, (&protocolASR{settings: c.settings}).maxPieceSeconds(), 1e-9)
		})
	}
}

func TestAFileTheVendorTakesGoesAsItIsWithoutDecoding(t *testing.T) {
	saved := ffmpegPath
	ffmpegPath = func() (string, error) { t.Fatal("nothing to convert or measure"); return "", nil }
	t.Cleanup(func() { ffmpegPath = saved })
	inner := &fakeTranscriber{}
	a := &protocolASR{inner: inner, modelName: "m", vendor: "V",
		settings: api.TranscriptionsSettings{Formats: []string{"wav", "mp3"}, MaxFileBytes: 25 << 20}}
	audio := []byte("an mp3, as far as anyone here knows")

	res, err := a.Transcribe(context.Background(), audio, "note.mp3")
	require.NoError(t, err)
	assert.Equal(t, "part1", res.Text)
	require.Len(t, inner.requests, 1)
	assert.Equal(t, "note.mp3", inner.requests[0].FileName)
	assert.Equal(t, audio, inner.requests[0].Audio)
}

func TestWithoutFFmpegAFormatTheVendorDoesNotTakeIsRefusedAsBefore(t *testing.T) {
	withoutFFmpeg(t)
	inner := &fakeTranscriber{}
	_, err := zhipuLike(inner).Transcribe(context.Background(), []byte("OggS"), "voice.ogg")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `glm-asr-2512 transcription: Zhipu BigModel accepts wav/mp3 audio, not "voice.ogg"`)
	assert.Contains(t, err.Error(), "ffmpeg")
	assert.Empty(t, inner.requests)
}

func TestWithoutFFmpegAFormatTheVendorTakesGoesWhateverItsLength(t *testing.T) {
	withoutFFmpeg(t)
	inner := &fakeTranscriber{}
	audio := []byte("an mp3 of unknown length")
	_, err := zhipuLike(inner).Transcribe(context.Background(), audio, "voice.mp3")
	require.NoError(t, err)
	require.Len(t, inner.requests, 1)
	assert.Equal(t, audio, inner.requests[0].Audio)
}

func TestAWhatsAppVoiceNoteIsConvertedToWAV(t *testing.T) {
	ogg := encode(t, pcmPattern(5, 5, 0), "ogg", "-c:a", "libopus", "-b:a", "24k")
	inner := &fakeTranscriber{}
	res, err := zhipuLike(inner).Transcribe(context.Background(), ogg, "voice.ogg")
	require.NoError(t, err)
	require.Len(t, inner.requests, 1)
	assert.Equal(t, "voice.wav", inner.requests[0].FileName)
	assert.InDelta(t, 5, wavSeconds(t, inner.requests[0].Audio), 0.1)
	assert.Equal(t, "part1", res.Text)
	assert.InDelta(t, 5, res.Duration, 0.1)
}

func TestARecordingOverTheDurationLimitIsSentInPieces(t *testing.T) {
	ogg := encode(t, pcmPattern(65, 5, 2), "ogg", "-c:a", "libopus", "-b:a", "24k")
	inner := &fakeTranscriber{answer: func(i int, _ api.TranscriptionRequest) (*api.Transcription, error) {
		return &api.Transcription{
			Text:     []string{"第一段，", "第二段，", "第三段。"}[i],
			Segments: []api.TranscriptionSegment{{Start: 0.5, End: 1.5, Text: "x"}},
		}, nil
	}}
	res, err := zhipuLike(inner).Transcribe(context.Background(), ogg, "voice.ogg")
	require.NoError(t, err)
	require.Len(t, inner.requests, 3)
	var starts []float64
	at := 0.0
	for i, req := range inner.requests {
		assert.Equal(t, fmt.Sprintf("voice-%d.wav", i+1), req.FileName)
		d := wavSeconds(t, req.Audio)
		assert.LessOrEqual(t, d, 29.0)
		starts = append(starts, at)
		at += d
	}
	assert.InDelta(t, 65, at, 0.1)
	assert.True(t, starts[1] > 26 && starts[1] < 28.1, "first cut at %.2f s", starts[1])
	assert.True(t, starts[2] > 54 && starts[2] < 56.1, "second cut at %.2f s", starts[2])
	assert.Equal(t, "第一段，第二段，第三段。", res.Text)
	require.Len(t, res.Segments, 3)
	for i, s := range res.Segments {
		assert.InDelta(t, starts[i]+0.5, s.Start, 1e-6)
	}
	assert.InDelta(t, 65, res.Duration, 0.1)
}

func TestAnMP3OverTheDurationLimitIsSentInPieces(t *testing.T) {
	mp3 := encode(t, pcmPattern(40, 5, 2), "mp3")
	inner := &fakeTranscriber{}
	res, err := zhipuLike(inner).Transcribe(context.Background(), mp3, "memo.mp3")
	require.NoError(t, err)
	require.Len(t, inner.requests, 2)
	for _, req := range inner.requests {
		assert.LessOrEqual(t, wavSeconds(t, req.Audio), 29.0)
	}
	assert.Equal(t, "part1 part2", res.Text)
}

func TestAShortMP3TheVendorTakesIsMeasuredThenSentAsItIs(t *testing.T) {
	mp3 := encode(t, pcmPattern(10, 5, 1), "mp3")
	inner := &fakeTranscriber{}
	_, err := zhipuLike(inner).Transcribe(context.Background(), mp3, "voice.mp3")
	require.NoError(t, err)
	require.Len(t, inner.requests, 1)
	assert.Equal(t, "voice.mp3", inner.requests[0].FileName)
	assert.Equal(t, mp3, inner.requests[0].Audio)
}

func TestOnePieceFailingFailsTheWhole(t *testing.T) {
	ogg := encode(t, pcmPattern(65, 5, 2), "ogg", "-c:a", "libopus")
	inner := &fakeTranscriber{answer: func(i int, _ api.TranscriptionRequest) (*api.Transcription, error) {
		if i == 1 {
			return nil, errors.New("HTTP 429")
		}
		return &api.Transcription{Text: "ok"}, nil
	}}
	_, err := zhipuLike(inner).Transcribe(context.Background(), ogg, "voice.ogg")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "piece 2 of 3")
	assert.Len(t, inner.requests, 2, "nothing is sent after the failure")
}

func TestAFileFFmpegCannotReadIsRefused(t *testing.T) {
	needFFmpeg(t)
	inner := &fakeTranscriber{}
	_, err := zhipuLike(inner).Transcribe(context.Background(), []byte("this is not audio"), "voice.ogg")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `accepts wav/mp3 audio, not "voice.ogg"`)
	assert.Contains(t, err.Error(), "ffmpeg could not read the .ogg audio")
	assert.Empty(t, inner.requests)
}

// A playlist or concat script dressed up as a voice note must not lead
// ffmpeg to another file: the demuxer is fixed by the extension.
func TestAPlaylistDressedAsAudioIsNotFollowed(t *testing.T) {
	needFFmpeg(t)
	secret := filepath.Join(t.TempDir(), "secret.wav")
	require.NoError(t, os.WriteFile(secret, wavFile(pcmPattern(3, 3, 0)), 0o600))
	for name, body := range map[string]string{
		"voice.ogg": "#EXTM3U\n#EXT-X-TARGETDURATION:3\n#EXTINF:3,\nfile:" + secret + "\n#EXT-X-ENDLIST\n",
		"voice.m4a": "ffconcat version 1.0\nfile '" + secret + "'\n",
		"note.mp3":  "#EXTM3U\n#EXTINF:3,\n" + secret + "\n",
	} {
		inner := &fakeTranscriber{}
		a := zhipuLike(inner)
		a.settings.Formats = []string{"wav"}
		_, err := a.Transcribe(context.Background(), []byte(body), name)
		require.Error(t, err, name)
		assert.Empty(t, inner.requests, name)
	}
}

func TestARecordingPastTheConversionBound(t *testing.T) {
	needFFmpeg(t)
	saved := maxPreparedSeconds
	maxPreparedSeconds = 20
	t.Cleanup(func() { maxPreparedSeconds = saved })
	pcm := pcmPattern(35, 5, 2)

	t.Run("in a format the vendor takes goes as it is", func(t *testing.T) {
		mp3 := encode(t, pcm, "mp3")
		inner := &fakeTranscriber{}
		_, err := zhipuLike(inner).Transcribe(context.Background(), mp3, "voice.mp3")
		require.NoError(t, err)
		require.Len(t, inner.requests, 1)
		assert.Equal(t, mp3, inner.requests[0].Audio)
	})
	t.Run("in another is refused", func(t *testing.T) {
		ogg := encode(t, pcm, "ogg", "-c:a", "libopus")
		inner := &fakeTranscriber{}
		_, err := zhipuLike(inner).Transcribe(context.Background(), ogg, "voice.ogg")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "longer than 20s")
		assert.Empty(t, inner.requests)
	})
}

// Through the catalog and over the wire: glm-asr's limits come from the
// Zhipu entry, and a 40-second voice note reaches it as two WAV files.
func TestZhipuReceivesAWhatsAppVoiceNoteAsWAVPieces(t *testing.T) {
	ogg := encode(t, pcmPattern(40, 5, 2), "ogg", "-c:a", "libopus", "-b:a", "24k")
	url, got, calls := upstream(t)
	a, err := NewASR(&Config{
		Source: types.ModelSourceRemote, Provider: "zhipu", BaseURL: url + "/api/paas/v4",
		ModelName: "glm-asr-2512", APIKey: "k",
	})
	require.NoError(t, err)
	res, err := a.Transcribe(context.Background(), ogg, "voice.ogg")
	require.NoError(t, err)
	assert.EqualValues(t, 2, calls.Load())
	assert.Equal(t, "/api/paas/v4/audio/transcriptions", got.path)
	assert.Equal(t, "voice-2.wav", got.fileName)
	assert.Equal(t, "hello hello", res.Text)
}
