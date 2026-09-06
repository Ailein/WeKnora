package whatsapp

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/Tencent/WeKnora/internal/im"
)

// The download cap must trigger on actual bytes written: the declared
// FileLength is sender-controlled and cannot be trusted.
func TestLimitedFileCapsWrites(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "limited-*")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	lf := &limitedFile{File: tmp, limit: 8}

	if _, err := lf.Write([]byte("12345678")); err != nil {
		t.Fatalf("write within limit failed: %v", err)
	}
	if _, err := lf.Write([]byte("9")); !errors.Is(err, errMediaTooLarge) {
		t.Fatalf("write past limit: got %v, want errMediaTooLarge", err)
	}
	if _, err := lf.WriteAt([]byte("xx"), 7); !errors.Is(err, errMediaTooLarge) {
		t.Fatalf("writeAt past limit: got %v, want errMediaTooLarge", err)
	}
	// In-place decryption rewrites earlier offsets; those must stay allowed.
	if _, err := lf.WriteAt([]byte("x"), 3); err != nil {
		t.Fatalf("writeAt within limit failed: %v", err)
	}
}

func TestTempFileReadCloserRemovesFile(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "cleanup-*")
	if err != nil {
		t.Fatal(err)
	}
	rc := &tempFileReadCloser{File: tmp}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(tmp.Name()); !os.IsNotExist(err) {
		t.Fatalf("temp file still exists after Close: %v", err)
	}
}

// The status API surfaces these transitions in the UI; terminal states must
// survive the trailing Disconnected event whatsmeow emits while tearing the
// socket down, or a dead pairing would show as a harmless reconnect.
func TestConnStateTransitions(t *testing.T) {
	a := newAdapter("ch-test", nil, nil)

	assertState := func(step, want string) {
		t.Helper()
		if got := a.ChannelStatus().State; got != want {
			t.Fatalf("%s: state = %q, want %q", step, got, want)
		}
	}

	assertState("initial", im.ChannelStateConnecting)

	a.trackConnState(&events.Connected{})
	assertState("after Connected", im.ChannelStateConnected)

	a.trackConnState(&events.Disconnected{})
	assertState("after Disconnected", im.ChannelStateConnecting)

	a.trackConnState(&events.Connected{})
	a.trackConnState(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	assertState("after LoggedOut", im.ChannelStateLoggedOut)

	a.trackConnState(&events.Disconnected{})
	assertState("Disconnected must not mask LoggedOut", im.ChannelStateLoggedOut)
}

func TestStreamReplacedIsTerminal(t *testing.T) {
	a := newAdapter("ch-test", nil, nil)
	a.trackConnState(&events.Connected{})
	a.trackConnState(&events.StreamReplaced{})
	a.trackConnState(&events.Disconnected{})
	if got := a.ChannelStatus().State; got != im.ChannelStateStreamReplaced {
		t.Fatalf("state = %q, want %q", got, im.ChannelStateStreamReplaced)
	}
}

// Media-cache keys must be scoped by chat and sender so another participant
// cannot overwrite a victim's pending entry by reusing their stanza ID.
func TestMediaKeyScopedBySender(t *testing.T) {
	chat := types.NewJID("12345-67890", types.GroupServer)
	mk := func(sender string) string {
		info := &types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: types.NewJID(sender, types.DefaultUserServer)},
			ID:            "3EB0BADC0FFEE",
		}
		return mediaKey(info)
	}
	if mk("111") == mk("222") {
		t.Error("same stanza ID from different senders must not collide")
	}
	if mk("111") != mk("111") {
		t.Error("key must be deterministic for the same sender")
	}
}

// whatsmeow streams the download with io.Copy, which prefers ReadFrom over
// Write. The cap has to hold on that path too, or the declared-small,
// actually-huge attachment lands on disk in full.
func TestLimitedFileCapsIoCopy(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "limited-copy-*")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	lf := &limitedFile{File: tmp, limit: 8}

	payload := bytes.Repeat([]byte("x"), 1000)
	if _, err := io.Copy(lf, bytes.NewReader(payload)); !errors.Is(err, errMediaTooLarge) {
		t.Fatalf("io.Copy past limit: got %v, want errMediaTooLarge", err)
	}
	info, err := tmp.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 8 {
		t.Fatalf("file grew to %d bytes despite the 8-byte cap", info.Size())
	}

	// Within the cap, io.Copy still works through the same path.
	small, err := os.CreateTemp(t.TempDir(), "limited-copy-small-*")
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	sf := &limitedFile{File: small, limit: 8}
	if n, err := io.Copy(sf, bytes.NewReader([]byte("12345678"))); err != nil || n != 8 {
		t.Fatalf("io.Copy within limit: n=%d err=%v", n, err)
	}
}
