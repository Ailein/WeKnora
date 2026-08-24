-- Rollback: mirrors versioned migration 000088_im_handoff.
ALTER TABLE im_channel_sessions DROP COLUMN handoff_notified_at;
ALTER TABLE im_channel_sessions DROP COLUMN consecutive_failures;
ALTER TABLE im_channels DROP COLUMN handoff_config;
