package im

import (
	"context"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	analyticsMaxDays      = 90
	analyticsDefaultDays  = 7
	analyticsTopUserLimit = 10
	// analyticsMaxTZOffsetMinutes bounds the client-supplied timezone offset
	// to the real-world UTC-14..UTC+14 range.
	analyticsMaxTZOffsetMinutes = 14 * 60
)

// AnalyticsQuery selects the window an IM analytics report covers.
type AnalyticsQuery struct {
	// Days is the number of local calendar days the report covers, ending today.
	Days int
	// TZOffsetMinutes is the viewer's UTC offset in minutes (east positive);
	// daily buckets are aligned to midnight in that timezone.
	TZOffsetMinutes int
	// IMChannelID optionally narrows the report to a single IM channel.
	IMChannelID string
}

// AnalyticsTotals aggregates the whole window.
type AnalyticsTotals struct {
	ActiveSessions   int64 `json:"active_sessions"`
	NewSessions      int64 `json:"new_sessions"`
	ActiveUsers      int64 `json:"active_users"`
	UserMessages     int64 `json:"user_messages"`
	BotReplies       int64 `json:"bot_replies"`
	ManualReplies    int64 `json:"manual_replies"`
	TakeoverSessions int64 `json:"takeover_sessions"`
	// HumanHandledNow is the number of conversations currently held by an
	// operator, regardless of the report window.
	HumanHandledNow int64 `json:"human_handled_now"`
	AvgBotReplyMs   int64 `json:"avg_bot_reply_ms"`
}

// AnalyticsDay is one local calendar day in the report.
type AnalyticsDay struct {
	Date          string `json:"date" gorm:"column:day"`
	UserMessages  int64  `json:"user_messages" gorm:"column:user_messages"`
	BotReplies    int64  `json:"bot_replies" gorm:"column:bot_replies"`
	ManualReplies int64  `json:"manual_replies" gorm:"column:manual_replies"`
	NewSessions   int64  `json:"new_sessions" gorm:"column:new_sessions"`
	ActiveUsers   int64  `json:"active_users" gorm:"column:active_users"`
}

// AnalyticsChannel is per-IM-channel traffic within the window.
type AnalyticsChannel struct {
	IMChannelID  string `json:"im_channel_id" gorm:"column:im_channel_id"`
	Name         string `json:"name" gorm:"-"`
	Platform     string `json:"platform" gorm:"-"`
	Sessions     int64  `json:"sessions" gorm:"column:sessions"`
	UserMessages int64  `json:"user_messages" gorm:"column:user_messages"`
}

// AnalyticsTopUser is one of the most talkative IM users within the window.
type AnalyticsTopUser struct {
	Platform string `json:"platform" gorm:"column:platform"`
	UserID   string `json:"user_id" gorm:"column:user_id"`
	Messages int64  `json:"messages" gorm:"column:messages"`
	Sessions int64  `json:"sessions" gorm:"column:sessions"`
	// LastActiveDate is the local calendar day of the user's latest message.
	LastActiveDate string `json:"last_active_date" gorm:"column:last_active"`
}

// AnalyticsResult is the full IM analytics report for one tenant.
type AnalyticsResult struct {
	StartDate string             `json:"start_date"`
	EndDate   string             `json:"end_date"`
	Days      int                `json:"days"`
	Totals    AnalyticsTotals    `json:"totals"`
	Daily     []AnalyticsDay     `json:"daily"`
	Channels  []AnalyticsChannel `json:"channels"`
	TopUsers  []AnalyticsTopUser `json:"top_users"`
}

// analyticsDialect renders the two SQL fragments that differ between
// PostgreSQL and SQLite: bucketing a stored timestamp into a local calendar
// day, and comparing a stored timestamp against a bound time.Time.
type analyticsDialect struct {
	sqlite bool
	offset int
}

// day converts a timestamp column to a 'YYYY-MM-DD' string in the viewer's
// timezone. The offset is a clamped int owned by this package, so inlining it
// into the SQL text is injection-safe.
func (d analyticsDialect) day(col string) string {
	if d.sqlite {
		// SQLite's date() normalizes any stored timezone suffix to UTC before
		// applying the modifier.
		return fmt.Sprintf("date(%s, '%+d minutes')", col, d.offset)
	}
	// The column is TIMESTAMP WITH TIME ZONE; strip to UTC wall time first so
	// the output does not depend on the connection's TimeZone setting.
	return fmt.Sprintf("to_char(%s AT TIME ZONE 'UTC' + interval '%d minutes', 'YYYY-MM-DD')", col, d.offset)
}

// after renders "col >= bound-param". SQLite compares timestamps as strings,
// so both sides go through datetime() to normalize timezone suffixes.
func (d analyticsDialect) after(col string) string {
	if d.sqlite {
		return fmt.Sprintf("datetime(%s) >= datetime(?)", col)
	}
	return fmt.Sprintf("%s >= ?", col)
}

// ChannelAnalytics builds the IM analytics report for a tenant. All numbers
// only cover messages that flowed through IM channels (message channel one of
// "im", "im_takeover", "im_manual"); console/web traffic is excluded.
func (s *Service) ChannelAnalytics(ctx context.Context, tenantID uint64, q AnalyticsQuery) (*AnalyticsResult, error) {
	days := q.Days
	if days <= 0 {
		days = analyticsDefaultDays
	}
	if days > analyticsMaxDays {
		days = analyticsMaxDays
	}
	offset := q.TZOffsetMinutes
	if offset > analyticsMaxTZOffsetMinutes {
		offset = analyticsMaxTZOffsetMinutes
	}
	if offset < -analyticsMaxTZOffsetMinutes {
		offset = -analyticsMaxTZOffsetMinutes
	}

	loc := time.FixedZone("im-analytics", offset*60)
	now := time.Now().In(loc)
	startLocal := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(days - 1))

	d := analyticsDialect{sqlite: s.db.Dialector.Name() == "sqlite", offset: offset}

	// Shared filter over IM-bound messages within the window.
	msgWhere := fmt.Sprintf(
		"cs.tenant_id = ? AND cs.deleted_at IS NULL AND m.deleted_at IS NULL AND %s AND m.channel IN (?, ?, ?)",
		d.after("m.created_at"),
	)
	msgArgs := []interface{}{tenantID, startLocal, types.ChannelIM, types.ChannelIMTakeover, types.ChannelIMManual}
	if q.IMChannelID != "" {
		msgWhere += " AND cs.im_channel_id = ?"
		msgArgs = append(msgArgs, q.IMChannelID)
	}

	result := &AnalyticsResult{
		StartDate: startLocal.Format("2006-01-02"),
		EndDate:   now.Format("2006-01-02"),
		Days:      days,
	}

	userMsg := fmt.Sprintf("m.role = 'user' AND m.channel IN ('%s', '%s')", types.ChannelIM, types.ChannelIMTakeover)
	botMsg := fmt.Sprintf("m.role = 'assistant' AND m.channel = '%s'", types.ChannelIM)
	manualMsg := fmt.Sprintf("m.role = 'assistant' AND m.channel = '%s'", types.ChannelIMManual)
	imUser := "cs.platform || ':' || cs.user_id"

	var totals struct {
		ActiveSessions   int64   `gorm:"column:active_sessions"`
		ActiveUsers      int64   `gorm:"column:active_users"`
		UserMessages     int64   `gorm:"column:user_messages"`
		BotReplies       int64   `gorm:"column:bot_replies"`
		ManualReplies    int64   `gorm:"column:manual_replies"`
		TakeoverSessions int64   `gorm:"column:takeover_sessions"`
		AvgBotReplyMs    float64 `gorm:"column:avg_bot_reply_ms"`
	}
	totalsSQL := fmt.Sprintf(`SELECT
		COUNT(DISTINCT m.session_id) AS active_sessions,
		COUNT(DISTINCT %s) AS active_users,
		COUNT(CASE WHEN %s THEN 1 END) AS user_messages,
		COUNT(CASE WHEN %s THEN 1 END) AS bot_replies,
		COUNT(CASE WHEN %s THEN 1 END) AS manual_replies,
		COUNT(DISTINCT CASE WHEN m.channel = '%s' THEN m.session_id END) AS takeover_sessions,
		COALESCE(AVG(CASE WHEN %s AND m.agent_duration_ms > 0 THEN m.agent_duration_ms END), 0) AS avg_bot_reply_ms
	FROM messages m
	JOIN im_channel_sessions cs ON cs.session_id = m.session_id
	WHERE %s`, imUser, userMsg, botMsg, manualMsg, types.ChannelIMTakeover, botMsg, msgWhere)
	if err := s.db.WithContext(ctx).Raw(totalsSQL, msgArgs...).Scan(&totals).Error; err != nil {
		return nil, fmt.Errorf("im analytics totals: %w", err)
	}
	result.Totals = AnalyticsTotals{
		ActiveSessions:   totals.ActiveSessions,
		ActiveUsers:      totals.ActiveUsers,
		UserMessages:     totals.UserMessages,
		BotReplies:       totals.BotReplies,
		ManualReplies:    totals.ManualReplies,
		TakeoverSessions: totals.TakeoverSessions,
		AvgBotReplyMs:    int64(totals.AvgBotReplyMs),
	}

	dailySQL := fmt.Sprintf(`SELECT
		%s AS day,
		COUNT(CASE WHEN %s THEN 1 END) AS user_messages,
		COUNT(CASE WHEN %s THEN 1 END) AS bot_replies,
		COUNT(CASE WHEN %s THEN 1 END) AS manual_replies,
		COUNT(DISTINCT CASE WHEN m.role = 'user' THEN %s END) AS active_users
	FROM messages m
	JOIN im_channel_sessions cs ON cs.session_id = m.session_id
	WHERE %s
	GROUP BY 1`, d.day("m.created_at"), userMsg, botMsg, manualMsg, imUser, msgWhere)
	var msgDays []AnalyticsDay
	if err := s.db.WithContext(ctx).Raw(dailySQL, msgArgs...).Scan(&msgDays).Error; err != nil {
		return nil, fmt.Errorf("im analytics daily messages: %w", err)
	}

	// New sessions come from the mapping table itself; a session counts on the
	// local day it was created.
	csWhere := fmt.Sprintf("cs.tenant_id = ? AND cs.deleted_at IS NULL AND %s", d.after("cs.created_at"))
	csArgs := []interface{}{tenantID, startLocal}
	if q.IMChannelID != "" {
		csWhere += " AND cs.im_channel_id = ?"
		csArgs = append(csArgs, q.IMChannelID)
	}
	newSessionsSQL := fmt.Sprintf(`SELECT %s AS day, COUNT(*) AS new_sessions
	FROM im_channel_sessions cs
	WHERE %s
	GROUP BY 1`, d.day("cs.created_at"), csWhere)
	var sessionDays []AnalyticsDay
	if err := s.db.WithContext(ctx).Raw(newSessionsSQL, csArgs...).Scan(&sessionDays).Error; err != nil {
		return nil, fmt.Errorf("im analytics daily sessions: %w", err)
	}

	byDay := make(map[string]*AnalyticsDay, len(msgDays))
	for i := range msgDays {
		byDay[msgDays[i].Date] = &msgDays[i]
	}
	for _, sd := range sessionDays {
		if row, ok := byDay[sd.Date]; ok {
			row.NewSessions = sd.NewSessions
		} else {
			row := sd
			byDay[sd.Date] = &row
		}
		result.Totals.NewSessions += sd.NewSessions
	}
	// Emit a contiguous series so the chart never has holes.
	result.Daily = make([]AnalyticsDay, 0, days)
	for cursor := startLocal; !cursor.After(now); cursor = cursor.AddDate(0, 0, 1) {
		key := cursor.Format("2006-01-02")
		if row, ok := byDay[key]; ok {
			result.Daily = append(result.Daily, *row)
		} else {
			result.Daily = append(result.Daily, AnalyticsDay{Date: key})
		}
	}

	humanSQL := fmt.Sprintf(`SELECT COUNT(*) FROM im_channel_sessions cs
	WHERE cs.tenant_id = ? AND cs.deleted_at IS NULL AND cs.handling_mode = ?
	AND (cs.handling_expires_at IS NULL OR %s)`, d.after("cs.handling_expires_at"))
	humanArgs := []interface{}{tenantID, HandlingModeHuman, time.Now()}
	if q.IMChannelID != "" {
		humanSQL += " AND cs.im_channel_id = ?"
		humanArgs = append(humanArgs, q.IMChannelID)
	}
	if err := s.db.WithContext(ctx).Raw(humanSQL, humanArgs...).Scan(&result.Totals.HumanHandledNow).Error; err != nil {
		return nil, fmt.Errorf("im analytics human handled: %w", err)
	}

	channelsSQL := fmt.Sprintf(`SELECT
		cs.im_channel_id AS im_channel_id,
		COUNT(DISTINCT m.session_id) AS sessions,
		COUNT(CASE WHEN %s THEN 1 END) AS user_messages
	FROM messages m
	JOIN im_channel_sessions cs ON cs.session_id = m.session_id
	WHERE %s
	GROUP BY cs.im_channel_id
	ORDER BY user_messages DESC`, userMsg, msgWhere)
	if err := s.db.WithContext(ctx).Raw(channelsSQL, msgArgs...).Scan(&result.Channels).Error; err != nil {
		return nil, fmt.Errorf("im analytics channels: %w", err)
	}
	if err := s.fillChannelMeta(ctx, result.Channels); err != nil {
		return nil, err
	}

	topUsersSQL := fmt.Sprintf(`SELECT
		cs.platform AS platform,
		cs.user_id AS user_id,
		COUNT(*) AS messages,
		COUNT(DISTINCT m.session_id) AS sessions,
		MAX(%s) AS last_active
	FROM messages m
	JOIN im_channel_sessions cs ON cs.session_id = m.session_id
	WHERE %s AND %s
	GROUP BY cs.platform, cs.user_id
	ORDER BY messages DESC
	LIMIT %d`, d.day("m.created_at"), msgWhere, userMsg, analyticsTopUserLimit)
	if err := s.db.WithContext(ctx).Raw(topUsersSQL, msgArgs...).Scan(&result.TopUsers).Error; err != nil {
		return nil, fmt.Errorf("im analytics top users: %w", err)
	}

	if result.Channels == nil {
		result.Channels = []AnalyticsChannel{}
	}
	if result.TopUsers == nil {
		result.TopUsers = []AnalyticsTopUser{}
	}
	return result, nil
}

// fillChannelMeta resolves channel names/platforms for the per-channel rows.
// Deleted channels are looked up too (soft delete keeps the row) so history
// stays labeled after a channel is removed.
func (s *Service) fillChannelMeta(ctx context.Context, channels []AnalyticsChannel) error {
	ids := make([]string, 0, len(channels))
	for _, ch := range channels {
		if ch.IMChannelID != "" {
			ids = append(ids, ch.IMChannelID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var metas []struct {
		ID       string `gorm:"column:id"`
		Name     string `gorm:"column:name"`
		Platform string `gorm:"column:platform"`
	}
	if err := s.db.WithContext(ctx).
		Raw("SELECT id, name, platform FROM im_channels WHERE id IN ?", ids).
		Scan(&metas).Error; err != nil {
		return fmt.Errorf("im analytics channel meta: %w", err)
	}
	byID := make(map[string]int, len(metas))
	for i, m := range metas {
		byID[m.ID] = i
	}
	for i := range channels {
		if j, ok := byID[channels[i].IMChannelID]; ok {
			channels[i].Name = metas[j].Name
			channels[i].Platform = metas[j].Platform
		}
	}
	return nil
}
