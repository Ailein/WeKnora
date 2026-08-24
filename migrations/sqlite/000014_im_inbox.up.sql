-- Mirrors versioned migration 000087_im_inbox:
-- operator inbox denormalized activity columns and quick-reply store.

ALTER TABLE im_channel_sessions ADD COLUMN peer_name TEXT NOT NULL DEFAULT '';
ALTER TABLE im_channel_sessions ADD COLUMN operator_unread_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE im_channel_sessions ADD COLUMN last_message_preview TEXT NOT NULL DEFAULT '';
ALTER TABLE im_channel_sessions ADD COLUMN last_message_role TEXT NOT NULL DEFAULT '';
ALTER TABLE im_channel_sessions ADD COLUMN last_message_at DATETIME;

UPDATE im_channel_sessions SET last_message_at = updated_at WHERE last_message_at IS NULL;

CREATE TABLE IF NOT EXISTS im_quick_replies (
    tenant_id INTEGER PRIMARY KEY,
    items TEXT NOT NULL DEFAULT '[]',
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
