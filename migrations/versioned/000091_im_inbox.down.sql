-- Rollback: 000089_im_inbox
DO $$ BEGIN RAISE NOTICE '[Migration 000091] Dropping IM operator inbox columns'; END $$;

DROP TABLE IF EXISTS im_quick_replies;

ALTER TABLE im_channel_sessions
    DROP COLUMN IF EXISTS last_message_at,
    DROP COLUMN IF EXISTS last_message_role,
    DROP COLUMN IF EXISTS last_message_preview,
    DROP COLUMN IF EXISTS operator_unread_count,
    DROP COLUMN IF EXISTS peer_name;

DO $$ BEGIN RAISE NOTICE '[Migration 000091] IM operator inbox columns dropped'; END $$;
