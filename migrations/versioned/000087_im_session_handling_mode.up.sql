-- Migration: 000087_im_session_handling_mode
-- Description: Human takeover state on IM channel sessions. handling_mode
-- switches a conversation between bot ('bot') and operator ('human') answering;
-- handling_expires_at/handling_timeout_minutes drive the auto-resume window.
DO $$ BEGIN RAISE NOTICE '[Migration 000087] Adding IM session handling (human takeover) columns'; END $$;

ALTER TABLE im_channel_sessions
    ADD COLUMN IF NOT EXISTS handling_mode VARCHAR(20) NOT NULL DEFAULT 'bot',
    ADD COLUMN IF NOT EXISTS handling_expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS handling_timeout_minutes INTEGER NOT NULL DEFAULT 0;

DO $$ BEGIN RAISE NOTICE '[Migration 000087] IM session handling columns added'; END $$;
