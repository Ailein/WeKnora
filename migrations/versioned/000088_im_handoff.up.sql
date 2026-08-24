-- Migration: 000088_im_handoff
-- Description: Automatic human-handoff triggers on IM channels. handoff_config
-- stores the per-channel trigger settings (keywords, fallback threshold,
-- auto-reply, notification webhook); consecutive_failures tracks the bot's
-- unanswered-message streak per conversation and handoff_notified_at
-- rate-limits repeated notifications for the same conversation.
DO $$ BEGIN RAISE NOTICE '[Migration 000088] Adding IM handoff trigger columns'; END $$;

ALTER TABLE im_channels
    ADD COLUMN IF NOT EXISTS handoff_config JSONB NOT NULL DEFAULT '{}';

ALTER TABLE im_channel_sessions
    ADD COLUMN IF NOT EXISTS consecutive_failures INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS handoff_notified_at TIMESTAMPTZ;

DO $$ BEGIN RAISE NOTICE '[Migration 000088] IM handoff trigger columns added'; END $$;
