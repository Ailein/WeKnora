-- Rollback: 000087_im_session_handling_mode

ALTER TABLE im_channel_sessions
    DROP COLUMN IF EXISTS handling_timeout_minutes,
    DROP COLUMN IF EXISTS handling_expires_at,
    DROP COLUMN IF EXISTS handling_mode;
