-- Migration: 000089_im_inbox
-- Description: Operator inbox for IM conversations. Denormalizes the latest
-- recorded message (preview/role/time) and an operator-unread counter onto
-- im_channel_sessions so the inbox list is a single-table query, captures the
-- peer's IM display name, and adds the per-tenant quick-reply store used by
-- the inbox composer.
DO $$ BEGIN RAISE NOTICE '[Migration 000089] Adding IM operator inbox columns'; END $$;

ALTER TABLE im_channel_sessions
    ADD COLUMN IF NOT EXISTS peer_name VARCHAR(255) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS operator_unread_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_message_preview VARCHAR(500) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_message_role VARCHAR(20) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_message_at TIMESTAMP WITH TIME ZONE;

-- Pre-existing conversations have no denormalized activity yet; seed the sort
-- key from the row's own updated_at so they still appear in the inbox in a
-- sensible order instead of clumping at the bottom.
UPDATE im_channel_sessions SET last_message_at = updated_at WHERE last_message_at IS NULL;

CREATE TABLE IF NOT EXISTS im_quick_replies (
    tenant_id BIGINT PRIMARY KEY,
    items JSONB NOT NULL DEFAULT '[]',
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

DO $$ BEGIN RAISE NOTICE '[Migration 000089] IM operator inbox columns added'; END $$;
