package whatsapp

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.mau.fi/whatsmeow"
)

func newWaitSession(id string) *pairingSession {
	return &pairingSession{
		id:        id,
		status:    PairingStatusWait,
		createdAt: time.Now(),
		firstQR:   make(chan struct{}),
	}
}

// finish must be terminal: once a session succeeded, a late error/timeout from
// the QR watcher must not overwrite the credentials the frontend is about to
// poll, and the QR payload (the pairing secret) must stop being served.
func TestPairingSessionTerminalStates(t *testing.T) {
	sess := newWaitSession("s-1")

	sess.setQR("data:image/png;base64,QQ==")
	snap := sess.snapshot()
	if snap.Status != PairingStatusWait || snap.QRPNG == "" {
		t.Fatalf("waiting snapshot = %+v", snap)
	}
	select {
	case <-sess.firstQR:
	default:
		t.Fatal("setQR did not unblock the first-QR waiter")
	}

	sess.finish(PairingStatusSuccess, "8613800138000:12@s.whatsapp.net", "8613800138000", "")
	snap = sess.snapshot()
	if snap.Status != PairingStatusSuccess || snap.DeviceJID == "" || snap.Phone != "8613800138000" {
		t.Fatalf("success snapshot = %+v", snap)
	}
	if snap.QRPNG != "" {
		t.Error("terminal snapshot still serves the QR payload")
	}

	sess.finish(PairingStatusError, "", "", "late failure")
	if snap = sess.snapshot(); snap.Status != PairingStatusSuccess || snap.Error != "" {
		t.Errorf("second finish overwrote the terminal state: %+v", snap)
	}
}

func TestPairingSessionFinishUnblocksWithoutQR(t *testing.T) {
	sess := newWaitSession("s-2")
	sess.finish(PairingStatusError, "", "", "connect failed")
	select {
	case <-sess.firstQR:
	default:
		t.Fatal("finish did not unblock StartPairing waiters")
	}
}

// snapshotLog records every status the session publishes, standing in for the
// Redis mirror.
type snapshotLog struct {
	mu    sync.Mutex
	snaps []*PairingStatus
}

func (l *snapshotLog) add(st *PairingStatus) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.snaps = append(l.snaps, st)
}

func (l *snapshotLog) first() *PairingStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.snaps) == 0 {
		return nil
	}
	return l.snaps[0]
}

func TestWatchQRChannel(t *testing.T) {
	run := func(t *testing.T, items []whatsmeow.QRChannelItem) (*pairingSession, *snapshotLog) {
		t.Helper()
		p := NewPairingService(nil, nil)
		log := &snapshotLog{}
		sess := newWaitSession("s-watch")
		sess.publish = log.add
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		qrChan := make(chan whatsmeow.QRChannelItem, len(items))
		for _, item := range items {
			qrChan <- item
		}
		close(qrChan)
		p.watch(ctx, sess, newOfflineClient(t), qrChan, cancel)
		return sess, log
	}

	t.Run("code renders a QR data URL", func(t *testing.T) {
		sess, log := run(t, []whatsmeow.QRChannelItem{{Event: "code", Code: "2@pairing-payload"}})
		first := log.first()
		if first == nil || first.Status != PairingStatusWait || !strings.HasPrefix(first.QRPNG, "data:image/png;base64,") {
			t.Fatalf("first published snapshot = %+v", first)
		}
		// The channel closed without success: the session must not stay "wait".
		if got := sess.snapshot(); got.Status != PairingStatusExpired {
			t.Errorf("status after channel close = %s", got.Status)
		}
	})

	t.Run("success captures device jid", func(t *testing.T) {
		sess, _ := run(t, []whatsmeow.QRChannelItem{whatsmeow.QRChannelSuccess})
		got := sess.snapshot()
		if got.Status != PairingStatusSuccess || got.Phone != testSelfPhone || !strings.Contains(got.DeviceJID, testSelfPhone) {
			t.Errorf("success snapshot = %+v", got)
		}
	})

	t.Run("timeout expires the session", func(t *testing.T) {
		sess, _ := run(t, []whatsmeow.QRChannelItem{whatsmeow.QRChannelTimeout})
		if got := sess.snapshot(); got.Status != PairingStatusExpired {
			t.Errorf("status = %s", got.Status)
		}
	})

	t.Run("pair error is reported", func(t *testing.T) {
		sess, _ := run(t, []whatsmeow.QRChannelItem{{Event: "error", Error: errors.New("pair rejected")}})
		got := sess.snapshot()
		if got.Status != PairingStatusError || got.Error != "pair rejected" {
			t.Errorf("snapshot = %+v", got)
		}
	})

	t.Run("bare channel close expires", func(t *testing.T) {
		sess, _ := run(t, nil)
		got := sess.snapshot()
		if got.Status != PairingStatusExpired || !strings.Contains(got.Error, "ended") {
			t.Errorf("snapshot = %+v", got)
		}
	})
}

// With multiple replicas behind a load balancer, Poll may land on an instance
// that does not own the live session; the Redis mirror must answer then.
func TestPairingPollFallsBackToRedisMirror(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rc.Close() })
	p := NewPairingService(nil, rc)

	st := &PairingStatus{
		SessionID: "wa-pair-remote",
		Status:    PairingStatusSuccess,
		DeviceJID: "8613800138000:12@s.whatsapp.net",
		Phone:     "8613800138000",
	}
	p.mirrorStatus(st)

	got, err := p.Poll("wa-pair-remote")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got.Status != PairingStatusSuccess || got.DeviceJID != st.DeviceJID || got.Phone != st.Phone {
		t.Errorf("mirrored status = %+v", got)
	}

	// The QR payload is a secret: the mirror must not outlive the retention.
	if ttl := mr.TTL(redisPairingKeyPrefix + "wa-pair-remote"); ttl <= 0 || ttl > sessionRetention {
		t.Errorf("mirror TTL = %s, want (0, %s]", ttl, sessionRetention)
	}

	if _, err := p.Poll("wa-pair-unknown"); err == nil {
		t.Error("unknown session should not resolve")
	}
}

func TestPairingPollPrefersLocalSession(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rc.Close() })
	p := NewPairingService(nil, rc)

	local := newWaitSession("wa-pair-local")
	local.qrPNG = "data:image/png;base64,LOCAL"
	p.sessions["wa-pair-local"] = local

	p.mirrorStatus(&PairingStatus{SessionID: "wa-pair-local", Status: PairingStatusExpired})

	got, err := p.Poll("wa-pair-local")
	if err != nil || got.Status != PairingStatusWait || got.QRPNG == "" {
		t.Errorf("poll = %+v, err %v (must serve the live local session)", got, err)
	}
}

// Lite mode has no Redis; everything must quietly stay process-local.
func TestPairingWithoutRedis(t *testing.T) {
	p := NewPairingService(nil, nil)
	p.mirrorStatus(&PairingStatus{SessionID: "s"})
	if _, err := p.Poll("missing"); err == nil {
		t.Error("missing session should error without redis")
	}
	p.sessions["s-local"] = newWaitSession("s-local")
	if got, err := p.Poll("s-local"); err != nil || got.Status != PairingStatusWait {
		t.Errorf("local poll = %+v, err %v", got, err)
	}
}

// prune must cancel abandoned in-flight pairings (each holds a live whatsmeow
// client and socket) and drop finished sessions after the retention window.
func TestPruneExpiredSessions(t *testing.T) {
	p := NewPairingService(nil, nil)

	cancelled := false
	old := newWaitSession("old-wait")
	old.createdAt = time.Now().Add(-sessionRetention - time.Minute)
	old.cancel = func() { cancelled = true }

	oldDone := newWaitSession("old-done")
	oldDone.createdAt = time.Now().Add(-sessionRetention - time.Minute)
	oldDone.finish(PairingStatusSuccess, "jid", "phone", "")

	fresh := newWaitSession("fresh")

	p.sessions = map[string]*pairingSession{"old-wait": old, "old-done": oldDone, "fresh": fresh}
	p.mu.Lock()
	p.prune()
	p.mu.Unlock()

	if !cancelled {
		t.Error("expired waiting session was not cancelled")
	}
	if _, ok := p.sessions["old-wait"]; ok {
		t.Error("expired waiting session still present")
	}
	if _, ok := p.sessions["old-done"]; ok {
		t.Error("expired finished session still present")
	}
	if _, ok := p.sessions["fresh"]; !ok {
		t.Error("fresh session was pruned")
	}
}

// The QR payload is the pairing secret: it must render locally into a real
// PNG data URL and never leave for a third-party renderer.
func TestQRPNGDataURL(t *testing.T) {
	url, err := qrPNGDataURL("2@secret-pairing-payload")
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(url, prefix) {
		t.Fatalf("not a PNG data URL: %.40s", url)
	}
	raw, err := base64.StdEncoding.DecodeString(url[len(prefix):])
	if err != nil {
		t.Fatalf("payload is not valid base64: %v", err)
	}
	if len(raw) < 8 || string(raw[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Error("decoded payload is not a PNG")
	}
}
