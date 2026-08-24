-- Mirrors versioned migration 000087_im_session_handling_mode:
-- human takeover state on IM channel sessions.

ALTER TABLE im_channel_sessions ADD COLUMN handling_mode VARCHAR(20) NOT NULL DEFAULT 'bot';
ALTER TABLE im_channel_sessions ADD COLUMN handling_expires_at DATETIME;
ALTER TABLE im_channel_sessions ADD COLUMN handling_timeout_minutes INTEGER NOT NULL DEFAULT 0;
