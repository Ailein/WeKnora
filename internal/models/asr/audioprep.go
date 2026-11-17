package asr

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Audio a vendor would refuse as it is — a container it does not list, a
// file over its size cap, a recording longer than it takes — is decoded with
// ffmpeg to 16 kHz mono PCM and sent as WAV, cut where needed at the quietest
// moment before each limit. WhatsApp voice notes are Ogg/Opus, for one, which
// Zhipu's glm-asr (wav or mp3, at most 30 seconds) turns away.

const (
	prepSampleRate     = 16000
	prepBytesPerSecond = prepSampleRate * 2 // 16-bit mono
	wavHeaderBytes     = 44
	// pieceMarginSeconds keeps a piece, or a file sent as it is, that far
	// inside a vendor's duration limit: its decoder and ours may not agree
	// to the frame on how long a recording is.
	pieceMarginSeconds = 1.0
	// A piece is cut at the quietest 20 ms in the last cutSearchSeconds
	// before its limit, which is usually a pause between words.
	cutFrameBytes    = prepBytesPerSecond / 50
	cutSearchSeconds = 8
	// silentFrameRMS: a piece none of whose 20 ms frames reaches this level
	// (about -50 dBFS) holds no speech.
	silentFrameRMS = 100.0
	// dataURIPrefixBytes is the longest "data:audio/...;base64," prefix of
	// the protocols that send the audio inside the request body.
	dataURIPrefixBytes = len("data:audio/flac;base64,")
	ffmpegTimeout      = 2 * time.Minute
)

// maxPreparedSeconds bounds what is decoded into memory: 30 minutes is about
// 58 MB of PCM. A variable so tests can lower it.
var maxPreparedSeconds = 30 * 60

// errNoConverter: this build cannot convert the audio, because ffmpeg is not
// installed or the extension names no container it is allowed to open.
var errNoConverter = errors.New("no audio converter for this file")

// ffmpegPath finds ffmpeg once; tests replace it.
var ffmpegPath = sync.OnceValues(func() (string, error) { return exec.LookPath("ffmpeg") })

// ffmpegDemuxers maps an extension to the one demuxer ffmpeg may read it
// with. Naming it, rather than letting ffmpeg probe, keeps playlist and concat
// formats, which open further files and URLs, away from an uploaded file.
var ffmpegDemuxers = map[string]string{
	"ogg": "ogg", "oga": "ogg", "opus": "ogg",
	"mp3": "mp3", "mpga": "mp3",
	"wav": "wav",
	"m4a": "mov", "mp4": "mov", "3gp": "mov",
	"aac":  "aac",
	"flac": "flac",
	"amr":  "amr",
	"webm": "matroska", "mka": "matroska",
	"aiff": "aiff", "aif": "aiff",
}

// decodePCM decodes audio to 16 kHz mono signed 16-bit PCM, at most
// maxPreparedSeconds of it; complete is false when the recording is longer
// and the PCM stops there.
func decodePCM(ctx context.Context, audio []byte, ext string) (pcm []byte, complete bool, err error) {
	demuxer, ok := ffmpegDemuxers[ext]
	if !ok {
		return nil, false, fmt.Errorf("%w: .%s", errNoConverter, ext)
	}
	bin, err := ffmpegPath()
	if err != nil {
		return nil, false, fmt.Errorf("%w: ffmpeg: %v", errNoConverter, err)
	}
	in, err := os.CreateTemp("", "weknora-asr-*."+ext)
	if err != nil {
		return nil, false, fmt.Errorf("convert audio: %w", err)
	}
	defer func() { _ = os.Remove(in.Name()) }()
	_, err = in.Write(audio)
	if closeErr := in.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, false, fmt.Errorf("convert audio: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, ffmpegTimeout)
	defer cancel()
	// One second past the bound shows the recording goes on.
	limit := maxPreparedSeconds + 1
	cmd := exec.CommandContext(ctx, bin,
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", "file", "-f", demuxer, "-i", "file:"+in.Name(),
		"-map", "0:a:0", "-vn", "-sn", "-dn",
		"-ac", "1", "-ar", strconv.Itoa(prepSampleRate), "-t", strconv.Itoa(limit),
		"-f", "s16le", "-acodec", "pcm_s16le", "pipe:1")
	out := &cappedBuffer{max: (limit + 1) * prepBytesPerSecond}
	stderr := &cappedBuffer{max: 2048, drop: true}
	cmd.Stdout, cmd.Stderr = out, stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, false, fmt.Errorf("ffmpeg could not read the .%s audio: %s", ext, detail)
	}
	pcm = out.Bytes()
	pcm = pcm[:len(pcm)&^1]
	if len(pcm) == 0 {
		return nil, false, fmt.Errorf("the .%s file holds no audio", ext)
	}
	if bound := maxPreparedSeconds * prepBytesPerSecond; len(pcm) > bound {
		return pcm[:bound], false, nil
	}
	return pcm, true, nil
}

// cappedBuffer holds at most max bytes. Past that it fails the write, which
// stops ffmpeg, or with drop set it keeps the start and discards the rest.
type cappedBuffer struct {
	bytes.Buffer
	max  int
	drop bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	room := b.max - b.Len()
	if len(p) <= room {
		return b.Buffer.Write(p)
	}
	if !b.drop {
		return 0, errors.New("decoded audio exceeds its bound")
	}
	if room > 0 {
		_, _ = b.Buffer.Write(p[:room])
	}
	return len(p), nil
}

// wavFile wraps 16 kHz mono 16-bit PCM in a WAV header.
func wavFile(pcm []byte) []byte {
	out := make([]byte, wavHeaderBytes+len(pcm))
	copy(out[0:], "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+len(pcm)))
	copy(out[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(out[16:], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(out[20:], 1)  // PCM
	binary.LittleEndian.PutUint16(out[22:], 1)  // mono
	binary.LittleEndian.PutUint32(out[24:], prepSampleRate)
	binary.LittleEndian.PutUint32(out[28:], prepBytesPerSecond)
	binary.LittleEndian.PutUint16(out[32:], 2)  // block align
	binary.LittleEndian.PutUint16(out[34:], 16) // bits per sample
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(len(pcm)))
	copy(out[wavHeaderBytes:], pcm)
	return out
}

// pieceBounds cuts pcm into consecutive [start, end) byte ranges no longer
// than maxSeconds, each ending at the quietest 20 ms within the last
// cutSearchSeconds before its limit (and never in the first half of the
// piece). maxSeconds <= 0 means no limit: one piece.
func pieceBounds(pcm []byte, maxSeconds float64) [][2]int {
	total := len(pcm) &^ 1
	maxBytes := int(maxSeconds*prepBytesPerSecond) &^ 1
	if maxSeconds <= 0 || total <= maxBytes {
		return [][2]int{{0, total}}
	}
	maxBytes = max(maxBytes, cutFrameBytes)
	var bounds [][2]int
	for start := 0; start < total; {
		end := start + maxBytes
		if end >= total {
			bounds = append(bounds, [2]int{start, total})
			break
		}
		lo := max(end-cutSearchSeconds*prepBytesPerSecond, start+maxBytes/2)
		cut, quietest := end, math.Inf(1)
		// Walking back from the limit, a later frame wins a tie: fewer pieces.
		for at := end - cutFrameBytes; at >= lo; at -= cutFrameBytes {
			if rms := frameRMS(pcm[at : at+cutFrameBytes]); rms < quietest {
				quietest, cut = rms, at+cutFrameBytes/2
			}
		}
		bounds = append(bounds, [2]int{start, cut})
		start = cut
	}
	return bounds
}

// audible keeps the pieces with sound in them: a piece of silence end to end,
// the pause after the last word say, would only cost a request. When no piece
// is audible all are kept and the vendor decides.
func audible(pcm []byte, bounds [][2]int) [][2]int {
	var kept [][2]int
	for _, b := range bounds {
		if !silent(pcm[b[0]:b[1]]) {
			kept = append(kept, b)
		}
	}
	if len(kept) == 0 {
		return bounds
	}
	return kept
}

func silent(pcm []byte) bool {
	for at := 0; at < len(pcm); at += cutFrameBytes {
		if frameRMS(pcm[at:min(at+cutFrameBytes, len(pcm))]) >= silentFrameRMS {
			return false
		}
	}
	return true
}

func frameRMS(frame []byte) float64 {
	n := len(frame) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		s := float64(int16(binary.LittleEndian.Uint16(frame[2*i:])))
		sum += s * s
	}
	return math.Sqrt(sum / float64(n))
}

// joinTranscripts joins the pieces' text: directly where either side is
// Chinese or Japanese, which put no spaces between words, with a space
// otherwise.
func joinTranscripts(parts []string) string {
	var b strings.Builder
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if b.Len() > 0 {
			last, _ := utf8.DecodeLastRuneInString(b.String())
			first, _ := utf8.DecodeRuneInString(part)
			if !unspaced(last) && !unspaced(first) {
				b.WriteByte(' ')
			}
		}
		b.WriteString(part)
	}
	return b.String()
}

func unspaced(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana) ||
		(r >= 0x3000 && r <= 0x303F) || // CJK punctuation
		(r >= 0xFF00 && r <= 0xFFEF) // full-width forms
}
