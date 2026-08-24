-- Rollback: 000088_im_handoff
DO $$ BEGIN RAISE NOTICE '[Migration 000088] Dropping IM handoff trigger columns'; END $$;

ALTER TABLE im_channel_sessions
    DROP COLUMN IF EXISTS handoff_notified_at,
    DROP COLUMN IF EXISTS consecutive_failures;

ALTER TABLE im_channels
    DROP COLUMN IF EXISTS handoff_config;

DO $$ BEGIN RAISE NOTICE '[Migration 000088] IM handoff trigger columns dropped'; END $$;
