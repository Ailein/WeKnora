-- Mirrors versioned migration 000086_im_handoff:
-- automatic human-handoff triggers on IM channels.

ALTER TABLE im_channels ADD COLUMN handoff_config TEXT NOT NULL DEFAULT '{}';
ALTER TABLE im_channel_sessions ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE im_channel_sessions ADD COLUMN handoff_notified_at DATETIME;
