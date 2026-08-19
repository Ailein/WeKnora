package whatsapp

import (
	"context"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/im"
	"github.com/Tencent/WeKnora/internal/logger"
)

// disconnectWait bounds how long the cancel function blocks for run() to
// close the socket. Disconnect is normally near-instant; the timeout only
// guards against a wedged connection holding up channel shutdown.
const disconnectWait = 5 * time.Second

// NewFactory returns an im.AdapterFactory for WhatsApp channels.
//
// Credentials:
//   - device_jid: JID of the linked device, filled in by the QR pairing flow
//     (e.g. "8613800138000:12@s.whatsapp.net"). Required.
//   - allow_from: comma-separated phone numbers allowed to DM the bot, or
//     "*" for everyone. Empty means DMs are rejected (fail-closed); group
//     messages always require an @mention of the bot.
//
// The channel runs in "websocket" mode so the service-level Redis leader
// election guarantees a single live socket per session across replicas —
// two concurrent connections with the same WhatsApp session corrupt its
// Signal state.
func NewFactory(db *gorm.DB) im.AdapterFactory {
	return func(factoryCtx context.Context, channel *im.IMChannel, msgHandler func(context.Context, *im.IncomingMessage) error) (im.Adapter, context.CancelFunc, error) {
		creds, err := im.ParseCredentials(channel.Credentials)
		if err != nil {
			return nil, nil, fmt.Errorf("parse whatsapp credentials: %w", err)
		}

		deviceJID := im.GetString(creds, "device_jid")
		if deviceJID == "" {
			return nil, nil, fmt.Errorf("whatsapp channel %s has no device_jid: pair a device by scanning the QR code first", channel.ID)
		}
		jid, err := types.ParseJID(deviceJID)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid device_jid %q: %w", deviceJID, err)
		}

		container, err := getContainer(db)
		if err != nil {
			return nil, nil, fmt.Errorf("whatsapp session store: %w", err)
		}
		device, err := container.GetDevice(factoryCtx, jid)
		if err != nil {
			return nil, nil, fmt.Errorf("load whatsapp device %s: %w", deviceJID, err)
		}
		if device == nil {
			return nil, nil, fmt.Errorf("whatsapp device %s not found in session store: re-scan the QR code", deviceJID)
		}

		client := whatsmeow.NewClient(device, newLogBridge(shortID(channel.ID)))
		adapter := newAdapter(channel.ID, client, parseAllowFrom(im.GetString(creds, "allow_from")))
		client.AddEventHandler(adapter.makeEventHandler(msgHandler))

		wsCtx, wsCancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			adapter.run(wsCtx)
		}()

		// Block until run() has really disconnected: the service releases the
		// Redis leader lock right after cancel returns, and another replica
		// must not open a second socket while this one lingers — concurrent
		// connections with the same WhatsApp session corrupt its Signal state.
		cancelFn := func() {
			wsCancel()
			select {
			case <-done:
			case <-time.After(disconnectWait):
				logger.Warnf(context.Background(),
					"[WhatsApp] Channel %s still disconnecting after %s; leader lock may briefly overlap", channel.ID, disconnectWait)
			}
		}
		return adapter, cancelFn, nil
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
