// QR pairing flow for WhatsApp channels.
//
// Flow (mirrors the WeChat QR login UX):
//  1. Admin calls StartPairing: a fresh unpaired whatsmeow client connects
//     and streams QR codes; the first code is returned as a PNG data URL.
//  2. Frontend polls Poll every second, re-rendering the PNG whenever the
//     code rotates (WhatsApp rotates codes every ~20-60s).
//  3. Admin scans the code with WhatsApp on their phone ("Linked devices").
//  4. On success Poll returns the device_jid; the frontend writes it into
//     the channel credentials and the channel factory takes over with a
//     fresh client for that device.
//
// The QR payload IS the pairing secret. It must be rendered locally
// (backend PNG → data URL) and never sent to third-party QR services.
package whatsapp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	qrcode "github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/logger"
)

const (
	// pairingTimeout bounds one pairing attempt end-to-end.
	pairingTimeout = 5 * time.Minute
	// firstQRTimeout is how long StartPairing waits for the first code.
	firstQRTimeout = 20 * time.Second
	// maxPendingPairings caps concurrent unfinished pairing sessions per
	// workspace, so one workspace cannot hold every pairing slot.
	maxPendingPairings = 3
	// sessionRetention keeps finished sessions around for the frontend to
	// read the terminal status before they are pruned.
	sessionRetention = 10 * time.Minute

	// redisPairingKeyPrefix stores cross-replica mirrors of pairing sessions.
	redisPairingKeyPrefix = "im:whatsapp:pairing:"
	// redisPairingOpTimeout bounds each best-effort Redis mirror operation.
	redisPairingOpTimeout = 3 * time.Second

	// pairedDeviceRetention is how long a completed pairing counts as proof
	// that the workspace controls the phone; the channel that claims the
	// device must be saved within it (afterwards a channel of the same
	// workspace that already carried the number is the other accepted proof).
	pairedDeviceRetention = 24 * time.Hour
	// redisPairedDevicePrefix mirrors device_jid → tenant across replicas.
	redisPairedDevicePrefix = "im:whatsapp:paired:"
)

// ErrPairingNotFound is returned by Poll for unknown, expired, or foreign
// (another workspace's) sessions — deliberately the same answer for all three.
var ErrPairingNotFound = errors.New("pairing session not found or expired")

// Pairing status values.
const (
	PairingStatusWait    = "wait"
	PairingStatusSuccess = "success"
	PairingStatusExpired = "expired"
	PairingStatusError   = "error"
)

// PairingStatus is the API-facing snapshot of a pairing session.
type PairingStatus struct {
	SessionID string `json:"session_id"`
	// TenantID is the workspace that started the pairing; only it may poll
	// the session. Carried by the Redis mirror, never echoed by the API.
	TenantID  uint64 `json:"tenant_id,omitempty"`
	Status    string `json:"status"`
	QRPNG     string `json:"qr_png,omitempty"`
	DeviceJID string `json:"device_jid,omitempty"`
	Phone     string `json:"phone,omitempty"`
	Error     string `json:"error,omitempty"`
}

type pairingSession struct {
	mu        sync.Mutex
	id        string
	tenantID  uint64
	status    string
	qrPNG     string
	deviceJID string
	phone     string
	errMsg    string
	createdAt time.Time
	firstQR   chan struct{}
	qrOnce    sync.Once
	// cancel and publish are set at construction time, before the session is
	// published into PairingService.sessions — prune() reads cancel under the
	// service mutex, so a later assignment would be a data race.
	cancel  context.CancelFunc
	publish func(*PairingStatus)
}

func (s *pairingSession) snapshot() *PairingStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &PairingStatus{
		SessionID: s.id,
		TenantID:  s.tenantID,
		Status:    s.status,
		DeviceJID: s.deviceJID,
		Phone:     s.phone,
		Error:     s.errMsg,
	}
	if s.status == PairingStatusWait {
		out.QRPNG = s.qrPNG
	}
	return out
}

func (s *pairingSession) setQR(png string) {
	s.mu.Lock()
	s.qrPNG = png
	s.mu.Unlock()
	s.qrOnce.Do(func() { close(s.firstQR) })
	s.publishSnapshot()
}

func (s *pairingSession) finish(status, deviceJID, phone, errMsg string) {
	s.mu.Lock()
	if s.status == PairingStatusWait {
		s.status = status
		s.deviceJID = deviceJID
		s.phone = phone
		s.errMsg = errMsg
		s.qrPNG = ""
	}
	s.mu.Unlock()
	// Unblock StartPairing waiters even if no code ever arrived.
	s.qrOnce.Do(func() { close(s.firstQR) })
	s.publishSnapshot()
}

func (s *pairingSession) publishSnapshot() {
	if s.publish != nil {
		s.publish(s.snapshot())
	}
}

// PairingService manages in-flight QR pairing sessions. One instance lives on
// the IM handler. The live whatsmeow client is process-local, but session
// status is mirrored to Redis (when available) so that Poll requests routed
// to another replica by a load balancer still see the session.
type PairingService struct {
	db       *gorm.DB
	redis    *redis.Client // nil in single-instance (Lite) mode
	mu       sync.Mutex
	sessions map[string]*pairingSession
	// paired remembers which workspace completed a pairing for a device JID
	// (process-local; mirrored to Redis). It is the proof the channel
	// handlers demand before a workspace may bind that JID.
	paired  map[string]pairedDevice
	counter int
}

type pairedDevice struct {
	tenantID uint64
	at       time.Time
}

func NewPairingService(db *gorm.DB, redisClient *redis.Client) *PairingService {
	return &PairingService{
		db:       db,
		redis:    redisClient,
		sessions: make(map[string]*pairingSession),
		paired:   make(map[string]pairedDevice),
	}
}

// newPairingSessionID returns an unguessable session id: the id is the only
// thing a poller presents, and a success snapshot carries the device JID.
func (p *PairingService) newPairingSessionID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		p.counter++
		return fmt.Sprintf("wa-pair-%d-%d", time.Now().UnixNano(), p.counter)
	}
	return "wa-pair-" + hex.EncodeToString(raw[:])
}

// StartPairing spawns a new pairing session for tenantID and returns once the
// first QR code is available (or a terminal state was reached before that).
func (p *PairingService) StartPairing(ctx context.Context, tenantID uint64) (*PairingStatus, error) {
	container, err := getContainer(p.db)
	if err != nil {
		return nil, fmt.Errorf("whatsapp session store: %w", err)
	}

	pairCtx, cancel := context.WithTimeout(context.Background(), pairingTimeout)

	p.mu.Lock()
	p.prune()
	pending := 0
	for _, s := range p.sessions {
		if s.tenantID == tenantID && s.snapshot().Status == PairingStatusWait {
			pending++
		}
	}
	if pending >= maxPendingPairings {
		p.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("too many pending pairing sessions (%d), finish or wait for them to expire", pending)
	}
	sess := &pairingSession{
		id:        p.newPairingSessionID(),
		tenantID:  tenantID,
		status:    PairingStatusWait,
		createdAt: time.Now(),
		firstQR:   make(chan struct{}),
		cancel:    cancel,
		publish:   p.mirrorStatus,
	}
	p.sessions[sess.id] = sess
	p.mu.Unlock()

	client := whatsmeow.NewClient(container.NewDevice(), newLogBridge("pairing"))
	qrChan, err := client.GetQRChannel(pairCtx)
	if err != nil {
		cancel()
		sess.finish(PairingStatusError, "", "", err.Error())
		return nil, fmt.Errorf("open QR channel: %w", err)
	}
	if err := client.Connect(); err != nil {
		cancel()
		sess.finish(PairingStatusError, "", "", err.Error())
		return nil, fmt.Errorf("connect for pairing: %w", err)
	}

	go p.watch(pairCtx, sess, client, qrChan, cancel)

	select {
	case <-sess.firstQR:
	case <-time.After(firstQRTimeout):
		cancel()
		sess.finish(PairingStatusError, "", "", "timed out waiting for the first QR code")
	case <-ctx.Done():
		cancel()
		sess.finish(PairingStatusError, "", "", "request cancelled")
	}
	return sess.snapshot(), nil
}

// Poll returns the current status of one of tenantID's pairing sessions. The
// live session is process-local; with multiple replicas behind a load
// balancer the poll may land elsewhere, so fall back to the Redis mirror
// before reporting not-found. Another workspace's session is "not found".
func (p *PairingService) Poll(tenantID uint64, sessionID string) (*PairingStatus, error) {
	p.mu.Lock()
	sess, ok := p.sessions[sessionID]
	p.prune()
	p.mu.Unlock()
	if ok {
		if sess.tenantID != tenantID {
			return nil, ErrPairingNotFound
		}
		return sess.snapshot(), nil
	}
	if st := p.lookupMirror(sessionID); st != nil {
		if st.TenantID != tenantID {
			return nil, ErrPairingNotFound
		}
		return st, nil
	}
	return nil, ErrPairingNotFound
}

// recordPairedDevice remembers that tenantID just proved control of the
// phone behind deviceJID by scanning the code.
func (p *PairingService) recordPairedDevice(deviceJID string, tenantID uint64) {
	if deviceJID == "" {
		return
	}
	p.mu.Lock()
	p.paired[deviceJID] = pairedDevice{tenantID: tenantID, at: time.Now()}
	p.mu.Unlock()
	if p.redis == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisPairingOpTimeout)
	defer cancel()
	if err := p.redis.Set(ctx, redisPairedDevicePrefix+deviceJID,
		strconv.FormatUint(tenantID, 10), pairedDeviceRetention).Err(); err != nil {
		logger.Warnf(ctx, "[WhatsApp] Mirror paired device %s to redis failed: %v", deviceJID, err)
	}
}

// PairedBy reports which workspace completed a pairing for deviceJID within
// pairedDeviceRetention, checking this process first and the Redis mirror
// (other replicas, restarts) second.
func (p *PairingService) PairedBy(ctx context.Context, deviceJID string) (uint64, bool) {
	if deviceJID == "" {
		return 0, false
	}
	p.mu.Lock()
	rec, ok := p.paired[deviceJID]
	p.mu.Unlock()
	if ok && time.Since(rec.at) < pairedDeviceRetention {
		return rec.tenantID, true
	}
	if p.redis == nil {
		return 0, false
	}
	rctx, cancel := context.WithTimeout(ctx, redisPairingOpTimeout)
	defer cancel()
	raw, err := p.redis.Get(rctx, redisPairedDevicePrefix+deviceJID).Result()
	if err != nil {
		return 0, false
	}
	tenantID, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return tenantID, true
}

// mirrorStatus mirrors a session snapshot to Redis. Best-effort: without
// Redis (or on write failure) pairing still works on the replica that owns
// the session. The QR payload is the pairing secret; it lives in Redis only
// for the short retention window, the same trust already extended to leader
// locks and dedup state.
func (p *PairingService) mirrorStatus(st *PairingStatus) {
	if p.redis == nil {
		return
	}
	payload, err := json.Marshal(st)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisPairingOpTimeout)
	defer cancel()
	if err := p.redis.Set(ctx, redisPairingKeyPrefix+st.SessionID, payload, sessionRetention).Err(); err != nil {
		logger.Warnf(ctx, "[WhatsApp] Mirror pairing session %s to redis failed: %v", st.SessionID, err)
	}
}

func (p *PairingService) lookupMirror(sessionID string) *PairingStatus {
	if p.redis == nil || sessionID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisPairingOpTimeout)
	defer cancel()
	payload, err := p.redis.Get(ctx, redisPairingKeyPrefix+sessionID).Bytes()
	if err != nil {
		return nil
	}
	st := &PairingStatus{}
	if err := json.Unmarshal(payload, st); err != nil {
		return nil
	}
	return st
}

// watch consumes QR channel events until pairing reaches a terminal state.
func (p *PairingService) watch(ctx context.Context, sess *pairingSession, client *whatsmeow.Client, qrChan <-chan whatsmeow.QRChannelItem, cancel context.CancelFunc) {
	terminal := false
	for item := range qrChan {
		switch item.Event {
		case "code":
			png, err := qrPNGDataURL(item.Code)
			if err != nil {
				logger.Errorf(ctx, "[WhatsApp] Render pairing QR failed: %v", err)
				continue
			}
			sess.setQR(png)
		case whatsmeow.QRChannelSuccess.Event:
			deviceJID, phone := "", ""
			if id := client.Store.ID; id != nil {
				deviceJID = id.String()
				phone = id.User
			}
			logger.Infof(ctx, "[WhatsApp] Pairing succeeded: jid=%s tenant=%d", deviceJID, sess.tenantID)
			p.recordPairedDevice(deviceJID, sess.tenantID)
			sess.finish(PairingStatusSuccess, deviceJID, phone, "")
			terminal = true
			// Let whatsmeow finish its post-pair handshake (key uploads,
			// app-state sync) before dropping this temporary client. The
			// channel factory reconnects with the persisted device later.
			go func() {
				time.Sleep(5 * time.Second)
				client.Disconnect()
				cancel()
			}()
		case whatsmeow.QRChannelTimeout.Event:
			sess.finish(PairingStatusExpired, "", "", "QR code expired before it was scanned")
			terminal = true
			client.Disconnect()
			cancel()
		default:
			msg := item.Event
			if item.Error != nil {
				msg = item.Error.Error()
			}
			sess.finish(PairingStatusError, "", "", msg)
			terminal = true
			client.Disconnect()
			cancel()
		}
	}
	if !terminal {
		sess.finish(PairingStatusExpired, "", "", "pairing session ended")
		client.Disconnect()
		cancel()
	}
}

// prune drops sessions past retention and paired-device records past theirs.
// Callers must hold p.mu.
func (p *PairingService) prune() {
	cutoff := time.Now().Add(-sessionRetention)
	for id, sess := range p.sessions {
		if sess.createdAt.Before(cutoff) {
			if snap := sess.snapshot(); snap.Status == PairingStatusWait && sess.cancel != nil {
				sess.cancel()
			}
			delete(p.sessions, id)
		}
	}
	pairedCutoff := time.Now().Add(-pairedDeviceRetention)
	for jid, rec := range p.paired {
		if rec.at.Before(pairedCutoff) {
			delete(p.paired, jid)
		}
	}
}

// qrPNGDataURL renders the QR payload locally. The payload is the pairing
// secret — it must never leave the process unrendered.
func qrPNGDataURL(code string) (string, error) {
	png, err := qrcode.Encode(code, qrcode.Medium, 512)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}
