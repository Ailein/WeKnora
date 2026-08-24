-- Rollback: mirrors versioned migration 000087_im_inbox.
DROP TABLE IF EXISTS im_quick_replies;
ALTER TABLE im_channel_sessions DROP COLUMN last_message_at;
ALTER TABLE im_channel_sessions DROP COLUMN last_message_role;
ALTER TABLE im_channel_sessions DROP COLUMN last_message_preview;
ALTER TABLE im_channel_sessions DROP COLUMN operator_unread_count;
ALTER TABLE im_channel_sessions DROP COLUMN peer_name;
