package im

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Tencent/WeKnora/internal/types"
)

// statusReportingAdapter is a webhook-shaped test adapter that also reports
// live connection health, like the whatsapp adapter does.
type statusReportingAdapter struct {
	lifecycleTestAdapter
	status ChannelStatus
}

func (a *statusReportingAdapter) ChannelStatus() ChannelStatus { return a.status }

func staticFactory(adapter Adapter) AdapterFactory {
	return func(context.Context, *IMChannel, func(context.Context, *IncomingMessage) error) (Adapter, context.CancelFunc, error) {
		return adapter, func() {}, nil
	}
}

func TestChannelRuntimeStatusFromLocalAdapter(t *testing.T) {
	db := newLifecycleTestDB(t)
	svc := newLifecycleTestService(db, nil, "inst-status-1")
	t.Cleanup(svc.Stop)

	reporter := &statusReportingAdapter{status: ChannelStatus{
		State:  ChannelStateConnected,
		Detail: "session up",
		Since:  time.Now(),
	}}
	svc.RegisterAdapterFactory("test", staticFactory(reporter))
	channel := createLifecycleChannel(t, db, "ch-status-live", "agent-status")
	if err := svc.StartChannel(channel); err != nil {
		t.Fatalf("start channel: %v", err)
	}

	st, ok := svc.ChannelRuntimeStatus("ch-status-live")
	if !ok || st.State != ChannelStateConnected || st.Detail != "session up" {
		t.Errorf("status = %+v ok=%v", st, ok)
	}
}

func TestChannelRuntimeStatusPlainAdapterIsRunning(t *testing.T) {
	db := newLifecycleTestDB(t)
	svc := newLifecycleTestService(db, nil, "inst-status-2")
	t.Cleanup(svc.Stop)

	svc.RegisterAdapterFactory("test", staticFactory(&lifecycleTestAdapter{}))
	channel := createLifecycleChannel(t, db, "ch-status-plain", "agent-status")
	if err := svc.StartChannel(channel); err != nil {
		t.Fatalf("start channel: %v", err)
	}

	st, ok := svc.ChannelRuntimeStatus("ch-status-plain")
	if !ok || st.State != ChannelStateRunning {
		t.Errorf("status = %+v ok=%v (webhook adapters report running)", st, ok)
	}
}

// A replica that does not own the long connection must answer from the Redis
// mirror, and report ok=false when no runtime exists anywhere.
func TestChannelRuntimeStatusMirrorFallback(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rc.Close() })
	svc := newLifecycleTestService(newLifecycleTestDB(t), rc, "inst-status-3")
	t.Cleanup(svc.Stop)

	if _, ok := svc.ChannelRuntimeStatus("ch-nowhere"); ok {
		t.Error("no local adapter and no mirror should report ok=false")
	}

	payload, err := json.Marshal(&ChannelStatus{State: ChannelStateConnecting, Detail: "reconnecting"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Set(context.Background(), RedisKeyChannelStatus+"ch-remote", payload, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	st, ok := svc.ChannelRuntimeStatus("ch-remote")
	if !ok || st.State != ChannelStateConnecting || st.Detail != "reconnecting" {
		t.Errorf("mirrored status = %+v ok=%v", st, ok)
	}
}

func TestMirrorChannelStatusPublishesWithTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rc.Close() })
	db := newLifecycleTestDB(t)
	svc := newLifecycleTestService(db, rc, "inst-status-4")
	t.Cleanup(svc.Stop)

	reporter := &statusReportingAdapter{status: ChannelStatus{State: ChannelStateConnected}}
	svc.RegisterAdapterFactory("test", staticFactory(reporter))
	channel := createLifecycleChannel(t, db, "ch-mirror", "agent-status")
	if err := svc.StartChannel(channel); err != nil {
		t.Fatalf("start channel: %v", err)
	}

	svc.mirrorChannelStatus("ch-mirror")

	raw, err := rc.Get(context.Background(), RedisKeyChannelStatus+"ch-mirror").Bytes()
	if err != nil {
		t.Fatalf("mirror not written: %v", err)
	}
	var st ChannelStatus
	if err := json.Unmarshal(raw, &st); err != nil || st.State != ChannelStateConnected {
		t.Errorf("mirrored payload = %s (err %v)", raw, err)
	}
	// Stale mirrors must expire on their own once the leader dies.
	if ttl := mr.TTL(RedisKeyChannelStatus + "ch-mirror"); ttl <= 0 || ttl > channelStatusTTL {
		t.Errorf("mirror TTL = %s, want (0, %s]", ttl, channelStatusTTL)
	}

	// Adapters without live state (plain webhook) publish nothing.
	svc.RegisterAdapterFactory("plain", staticFactory(&lifecycleTestAdapter{}))
	plain := &IMChannel{
		ID: "ch-plain", TenantID: 1, AgentID: "agent-status", Platform: "plain",
		Enabled: true, Mode: "webhook", OutputMode: "full",
		SessionMode: string(SessionModeUser), Credentials: types.JSON(`{}`),
	}
	if err := svc.StartChannel(plain); err != nil {
		t.Fatalf("start plain channel: %v", err)
	}
	svc.mirrorChannelStatus("ch-plain")
	if err := rc.Get(context.Background(), RedisKeyChannelStatus+"ch-plain").Err(); !errors.Is(err, redis.Nil) {
		t.Errorf("plain adapter mirrored a status: err=%v", err)
	}
}

// The status API explains why an enabled channel is not running; the record
// must survive until the next successful start and then clear.
func TestLastStartErrorRecordsAndClears(t *testing.T) {
	db := newLifecycleTestDB(t)
	svc := newLifecycleTestService(db, nil, "inst-status-5")
	t.Cleanup(svc.Stop)
	channel := createLifecycleChannel(t, db, "ch-err", "agent-status")

	svc.RegisterAdapterFactory("test", func(context.Context, *IMChannel, func(context.Context, *IncomingMessage) error) (Adapter, context.CancelFunc, error) {
		return nil, nil, fmt.Errorf("device not paired")
	})
	if err := svc.StartChannel(channel); err == nil {
		t.Fatal("factory failure should surface as an error")
	}
	if got := svc.LastStartError("ch-err"); !strings.Contains(got, "device not paired") {
		t.Errorf("LastStartError = %q", got)
	}

	svc.RegisterAdapterFactory("test", staticFactory(&lifecycleTestAdapter{}))
	if err := svc.StartChannel(channel); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if got := svc.LastStartError("ch-err"); got != "" {
		t.Errorf("LastStartError after success = %q, want empty", got)
	}

	ghost := &IMChannel{
		ID: "ch-ghost", TenantID: 1, AgentID: "agent-status", Platform: "ghost",
		Enabled: true, Mode: "webhook", OutputMode: "full",
		SessionMode: string(SessionModeUser), Credentials: types.JSON(`{}`),
	}
	if err := svc.StartChannel(ghost); err == nil {
		t.Fatal("unknown platform should fail")
	}
	if got := svc.LastStartError("ch-ghost"); !strings.Contains(got, "no adapter factory") {
		t.Errorf("LastStartError = %q", got)
	}
}
