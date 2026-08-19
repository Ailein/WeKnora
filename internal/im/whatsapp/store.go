// Package whatsapp implements the WhatsApp IM channel using whatsmeow,
// a Go implementation of the WhatsApp Web multidevice protocol.
//
// Unlike the official Cloud API, this links to a regular WhatsApp account
// as a companion device via QR-code pairing (the same approach as the
// Baileys library). Session material is persisted by whatsmeow's sqlstore
// in the main application database (whatsmeow_* tables, self-migrated),
// so no extra deployment component or volume is needed.
package whatsapp

import (
	"context"
	"fmt"
	"sync"

	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"gorm.io/gorm"
)

var (
	containerMu   sync.Mutex
	containerInst *sqlstore.Container
)

// getContainer returns the process-wide whatsmeow session store backed by the
// application database. The first successful call runs whatsmeow's own schema
// migration (whatsmeow_* tables), which is managed by the library, not by the
// project's migrations directory. Only success is cached: a transient database
// error at startup must not disable WhatsApp until the next process restart.
func getContainer(db *gorm.DB) (*sqlstore.Container, error) {
	containerMu.Lock()
	defer containerMu.Unlock()
	if containerInst != nil {
		return containerInst, nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql.DB from gorm: %w", err)
	}
	dialect := db.Dialector.Name()
	// whatsmeow expects "sqlite3" while gorm reports "sqlite".
	if dialect == "sqlite" {
		dialect = "sqlite3"
	}
	container := sqlstore.NewWithDB(sqlDB, dialect, newLogBridge("store"))
	if err := container.Upgrade(context.Background()); err != nil {
		return nil, fmt.Errorf("upgrade whatsmeow store schema: %w", err)
	}
	containerInst = container
	return containerInst, nil
}

// DeviceExists reports whether the paired device is still present in the
// whatsmeow session store. whatsmeow deletes the stored session when the
// account unlinks the device (401/LoggedOut), so a missing row is the durable
// "re-scan the QR code" signal the status API relies on.
func DeviceExists(ctx context.Context, db *gorm.DB, deviceJID string) (bool, error) {
	jid, err := types.ParseJID(deviceJID)
	if err != nil {
		return false, fmt.Errorf("invalid device_jid %q: %w", deviceJID, err)
	}
	container, err := getContainer(db)
	if err != nil {
		return false, err
	}
	device, err := container.GetDevice(ctx, jid)
	if err != nil {
		return false, err
	}
	return device != nil, nil
}
