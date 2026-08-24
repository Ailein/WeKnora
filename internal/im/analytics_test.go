package im

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Lightweight row types matching only the columns the analytics queries touch;
// the production structs carry PostgreSQL-only defaults SQLite cannot migrate.

type analyticsTestMessage struct {
	ID              string `gorm:"primaryKey"`
	SessionID       string
	Role            string
	Channel         string
	AgentDurationMs int64
	CreatedAt       time.Time
	DeletedAt       gorm.DeletedAt
}

func (analyticsTestMessage) TableName() string { return "messages" }

type analyticsTestChannelSession struct {
	ID                string `gorm:"primaryKey"`
	Platform          string
	UserID            string
	SessionID         string
	TenantID          uint64
	IMChannelID       string
	HandlingMode      string
	HandlingExpiresAt *time.Time
	CreatedAt         time.Time
	DeletedAt         gorm.DeletedAt
}

func (analyticsTestChannelSession) TableName() string { return "im_channel_sessions" }

type analyticsTestIMChannel struct {
	ID       string `gorm:"primaryKey"`
	Name     string
	Platform string
}

func (analyticsTestIMChannel) TableName() string { return "im_channels" }

func newAnalyticsTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:im-analytics-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&analyticsTestMessage{}, &analyticsTestChannelSession{}, &analyticsTestIMChannel{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedAnalyticsFixture loads two tenants of IM traffic anchored to the current
// day so the report window logic is exercised against real "now".
//
// Tenant 1:
//   - channel A (whatsapp "WA Support"): user u1/session s1 created today with
//     3 QA rounds (durations 1000/2000/3000ms) plus a console web message that
//     must be ignored; user u2/session s2 created yesterday with 1 QA round
//     (500ms), one takeover-recorded user message, one manual operator reply,
//     currently held by an operator.
//   - channel B (telegram): user u3/session s3 created 10 days ago with 1 QA
//     round — outside a 7-day window, inside a 30-day one.
//
// Tenant 2: one session s4 today that must never leak into tenant 1 reports.
func seedAnalyticsFixture(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now().UTC()
	today := now.Add(-2 * time.Hour) // stay inside "today" for any tz offset of 0
	if now.Hour() < 2 {
		today = now // early UTC morning: now itself is the safest "today" stamp
	}
	yesterday := today.AddDate(0, 0, -1)
	tenDaysAgo := today.AddDate(0, 0, -10)

	channels := []analyticsTestIMChannel{
		{ID: "chan-a", Name: "WA Support", Platform: "whatsapp"},
		{ID: "chan-b", Name: "TG Bot", Platform: "telegram"},
	}
	expires := now.Add(30 * time.Minute)
	sessions := []analyticsTestChannelSession{
		{ID: "cs1", Platform: "whatsapp", UserID: "u1", SessionID: "s1", TenantID: 1, IMChannelID: "chan-a", HandlingMode: HandlingModeBot, CreatedAt: today},
		{ID: "cs2", Platform: "whatsapp", UserID: "u2", SessionID: "s2", TenantID: 1, IMChannelID: "chan-a", HandlingMode: HandlingModeHuman, HandlingExpiresAt: &expires, CreatedAt: yesterday},
		{ID: "cs3", Platform: "telegram", UserID: "u3", SessionID: "s3", TenantID: 1, IMChannelID: "chan-b", HandlingMode: HandlingModeBot, CreatedAt: tenDaysAgo},
		{ID: "cs4", Platform: "whatsapp", UserID: "other", SessionID: "s4", TenantID: 2, IMChannelID: "chan-x", HandlingMode: HandlingModeHuman, CreatedAt: today},
	}
	var messages []analyticsTestMessage
	addRound := func(session string, at time.Time, n int, durMs int64) {
		messages = append(messages,
			analyticsTestMessage{ID: fmt.Sprintf("%s-u%d", session, n), SessionID: session, Role: "user", Channel: "im", CreatedAt: at},
			analyticsTestMessage{ID: fmt.Sprintf("%s-a%d", session, n), SessionID: session, Role: "assistant", Channel: "im", AgentDurationMs: durMs, CreatedAt: at.Add(time.Second)},
		)
	}
	addRound("s1", today, 1, 1000)
	addRound("s1", today.Add(time.Minute), 2, 2000)
	addRound("s1", today.Add(2*time.Minute), 3, 3000)
	addRound("s2", yesterday, 1, 500)
	addRound("s3", tenDaysAgo, 1, 700)
	addRound("s4", today, 1, 900)
	messages = append(messages,
		// Console traffic on an IM-bound session must not count.
		analyticsTestMessage{ID: "s1-web", SessionID: "s1", Role: "user", Channel: "web", CreatedAt: today},
		// Takeover-recorded user message and the operator's manual reply.
		analyticsTestMessage{ID: "s2-t1", SessionID: "s2", Role: "user", Channel: "im_takeover", CreatedAt: yesterday.Add(time.Minute)},
		analyticsTestMessage{ID: "s2-m1", SessionID: "s2", Role: "assistant", Channel: "im_manual", CreatedAt: yesterday.Add(2 * time.Minute)},
	)

	if err := db.Create(&channels).Error; err != nil {
		t.Fatalf("seed channels: %v", err)
	}
	if err := db.Create(&sessions).Error; err != nil {
		t.Fatalf("seed sessions: %v", err)
	}
	if err := db.Create(&messages).Error; err != nil {
		t.Fatalf("seed messages: %v", err)
	}
}

func TestChannelAnalyticsSevenDays(t *testing.T) {
	db := newAnalyticsTestDB(t)
	seedAnalyticsFixture(t, db)
	svc := &Service{db: db}

	res, err := svc.ChannelAnalytics(context.Background(), 1, AnalyticsQuery{Days: 7})
	if err != nil {
		t.Fatalf("ChannelAnalytics: %v", err)
	}

	tot := res.Totals
	if tot.ActiveSessions != 2 {
		t.Errorf("active_sessions = %d, want 2", tot.ActiveSessions)
	}
	if tot.ActiveUsers != 2 {
		t.Errorf("active_users = %d, want 2", tot.ActiveUsers)
	}
	// s1: 3 im user messages; s2: 1 im + 1 im_takeover. The web message is excluded.
	if tot.UserMessages != 5 {
		t.Errorf("user_messages = %d, want 5", tot.UserMessages)
	}
	if tot.BotReplies != 4 {
		t.Errorf("bot_replies = %d, want 4", tot.BotReplies)
	}
	if tot.ManualReplies != 1 {
		t.Errorf("manual_replies = %d, want 1", tot.ManualReplies)
	}
	if tot.TakeoverSessions != 1 {
		t.Errorf("takeover_sessions = %d, want 1", tot.TakeoverSessions)
	}
	if tot.NewSessions != 2 {
		t.Errorf("new_sessions = %d, want 2", tot.NewSessions)
	}
	if tot.HumanHandledNow != 1 {
		t.Errorf("human_handled_now = %d, want 1", tot.HumanHandledNow)
	}
	// (1000+2000+3000+500)/4
	if tot.AvgBotReplyMs != 1625 {
		t.Errorf("avg_bot_reply_ms = %d, want 1625", tot.AvgBotReplyMs)
	}

	if len(res.Daily) != 7 {
		t.Fatalf("daily length = %d, want 7", len(res.Daily))
	}
	last := res.Daily[len(res.Daily)-1]
	if last.UserMessages != 3 || last.BotReplies != 3 || last.NewSessions != 1 || last.ActiveUsers != 1 {
		t.Errorf("today bucket = %+v, want 3 user / 3 bot / 1 new / 1 active", last)
	}
	prev := res.Daily[len(res.Daily)-2]
	if prev.UserMessages != 2 || prev.BotReplies != 1 || prev.ManualReplies != 1 || prev.NewSessions != 1 {
		t.Errorf("yesterday bucket = %+v, want 2 user / 1 bot / 1 manual / 1 new", prev)
	}
	if res.Daily[0].UserMessages != 0 {
		t.Errorf("oldest bucket should be empty, got %+v", res.Daily[0])
	}

	if len(res.Channels) != 1 {
		t.Fatalf("channels length = %d, want 1 (%+v)", len(res.Channels), res.Channels)
	}
	ch := res.Channels[0]
	if ch.IMChannelID != "chan-a" || ch.Name != "WA Support" || ch.Platform != "whatsapp" || ch.Sessions != 2 || ch.UserMessages != 5 {
		t.Errorf("channel row = %+v", ch)
	}

	if len(res.TopUsers) != 2 {
		t.Fatalf("top_users length = %d, want 2", len(res.TopUsers))
	}
	if res.TopUsers[0].UserID != "u1" || res.TopUsers[0].Messages != 3 || res.TopUsers[0].Sessions != 1 {
		t.Errorf("top user[0] = %+v", res.TopUsers[0])
	}
	if res.TopUsers[1].UserID != "u2" || res.TopUsers[1].Messages != 2 {
		t.Errorf("top user[1] = %+v", res.TopUsers[1])
	}
	if res.TopUsers[0].LastActiveDate == "" {
		t.Errorf("top user last_active_date should be set")
	}
}

func TestChannelAnalyticsThirtyDaysAndChannelFilter(t *testing.T) {
	db := newAnalyticsTestDB(t)
	seedAnalyticsFixture(t, db)
	svc := &Service{db: db}

	res, err := svc.ChannelAnalytics(context.Background(), 1, AnalyticsQuery{Days: 30})
	if err != nil {
		t.Fatalf("ChannelAnalytics: %v", err)
	}
	if res.Totals.ActiveSessions != 3 || res.Totals.ActiveUsers != 3 || res.Totals.UserMessages != 6 {
		t.Errorf("30d totals = %+v, want 3 sessions / 3 users / 6 user messages", res.Totals)
	}
	if len(res.Daily) != 30 {
		t.Errorf("daily length = %d, want 30", len(res.Daily))
	}
	if len(res.Channels) != 2 {
		t.Fatalf("channels length = %d, want 2", len(res.Channels))
	}

	filtered, err := svc.ChannelAnalytics(context.Background(), 1, AnalyticsQuery{Days: 30, IMChannelID: "chan-b"})
	if err != nil {
		t.Fatalf("ChannelAnalytics filtered: %v", err)
	}
	ft := filtered.Totals
	if ft.ActiveSessions != 1 || ft.UserMessages != 1 || ft.BotReplies != 1 || ft.NewSessions != 1 {
		t.Errorf("chan-b totals = %+v, want 1/1/1/1", ft)
	}
	if ft.HumanHandledNow != 0 {
		t.Errorf("chan-b human_handled_now = %d, want 0", ft.HumanHandledNow)
	}
	if len(filtered.TopUsers) != 1 || filtered.TopUsers[0].UserID != "u3" {
		t.Errorf("chan-b top users = %+v", filtered.TopUsers)
	}
}

func TestChannelAnalyticsTenantIsolationAndEmpty(t *testing.T) {
	db := newAnalyticsTestDB(t)
	seedAnalyticsFixture(t, db)
	svc := &Service{db: db}

	res, err := svc.ChannelAnalytics(context.Background(), 2, AnalyticsQuery{Days: 7})
	if err != nil {
		t.Fatalf("ChannelAnalytics: %v", err)
	}
	if res.Totals.UserMessages != 1 || res.Totals.ActiveSessions != 1 || res.Totals.HumanHandledNow != 1 {
		t.Errorf("tenant 2 totals = %+v", res.Totals)
	}

	empty, err := svc.ChannelAnalytics(context.Background(), 99, AnalyticsQuery{})
	if err != nil {
		t.Fatalf("ChannelAnalytics empty: %v", err)
	}
	if empty.Totals != (AnalyticsTotals{}) {
		t.Errorf("empty totals = %+v, want zero value", empty.Totals)
	}
	if len(empty.Daily) != analyticsDefaultDays {
		t.Errorf("empty daily length = %d, want %d", len(empty.Daily), analyticsDefaultDays)
	}
	if empty.Channels == nil || empty.TopUsers == nil {
		t.Errorf("empty slices must be non-nil for JSON: channels=%v topUsers=%v", empty.Channels, empty.TopUsers)
	}
}

func TestChannelAnalyticsTimezoneBuckets(t *testing.T) {
	db := newAnalyticsTestDB(t)
	svc := &Service{db: db}

	// One message 30 minutes before UTC midnight: it belongs to "yesterday" at
	// UTC and to "today" at UTC+1.
	utcNow := time.Now().UTC()
	midnight := time.Date(utcNow.Year(), utcNow.Month(), utcNow.Day(), 0, 0, 0, 0, time.UTC)
	at := midnight.Add(-30 * time.Minute)
	if err := db.Create(&analyticsTestChannelSession{
		ID: "cs-tz", Platform: "whatsapp", UserID: "tz-user", SessionID: "s-tz",
		TenantID: 7, IMChannelID: "chan-a", HandlingMode: HandlingModeBot, CreatedAt: at,
	}).Error; err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if err := db.Create(&analyticsTestMessage{
		ID: "m-tz", SessionID: "s-tz", Role: "user", Channel: "im", CreatedAt: at,
	}).Error; err != nil {
		t.Fatalf("seed message: %v", err)
	}

	bucketFor := func(offsetMinutes int) string {
		t.Helper()
		res, err := svc.ChannelAnalytics(context.Background(), 7, AnalyticsQuery{Days: 3, TZOffsetMinutes: offsetMinutes})
		if err != nil {
			t.Fatalf("ChannelAnalytics offset %d: %v", offsetMinutes, err)
		}
		for _, day := range res.Daily {
			if day.UserMessages == 1 {
				return day.Date
			}
		}
		t.Fatalf("offset %d: message not found in daily buckets %+v", offsetMinutes, res.Daily)
		return ""
	}

	utcBucket := bucketFor(0)
	if want := at.Format("2006-01-02"); utcBucket != want {
		t.Errorf("UTC bucket = %s, want %s", utcBucket, want)
	}
	shifted := bucketFor(60)
	if want := at.Add(time.Hour).Format("2006-01-02"); shifted != want {
		t.Errorf("UTC+1 bucket = %s, want %s", shifted, want)
	}
	if shifted == utcBucket {
		t.Errorf("offset should move the message across midnight: both %s", shifted)
	}
}

func TestChannelAnalyticsClampsQuery(t *testing.T) {
	db := newAnalyticsTestDB(t)
	svc := &Service{db: db}

	res, err := svc.ChannelAnalytics(context.Background(), 1, AnalyticsQuery{Days: 500, TZOffsetMinutes: 99999})
	if err != nil {
		t.Fatalf("ChannelAnalytics: %v", err)
	}
	if res.Days != analyticsMaxDays || len(res.Daily) != analyticsMaxDays {
		t.Errorf("days = %d, daily = %d, want both %d", res.Days, len(res.Daily), analyticsMaxDays)
	}

	res, err = svc.ChannelAnalytics(context.Background(), 1, AnalyticsQuery{Days: -3, TZOffsetMinutes: -99999})
	if err != nil {
		t.Fatalf("ChannelAnalytics: %v", err)
	}
	if res.Days != analyticsDefaultDays {
		t.Errorf("days = %d, want default %d", res.Days, analyticsDefaultDays)
	}
}
