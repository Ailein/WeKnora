package whatsapp

import (
	"context"
	"fmt"

	"github.com/Tencent/WeKnora/internal/logger"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// logBridge adapts whatsmeow's waLog.Logger to the project logger.
// whatsmeow is chatty at INFO level, so INFO is demoted to debug and
// DEBUG is dropped entirely; WARN/ERROR pass through.
type logBridge struct {
	module string
}

var _ waLog.Logger = (*logBridge)(nil)

func newLogBridge(module string) waLog.Logger {
	return &logBridge{module: module}
}

func (l *logBridge) prefix(msg string) string {
	return "[WhatsApp/" + l.module + "] " + msg
}

func (l *logBridge) Errorf(msg string, args ...any) {
	logger.Errorf(context.Background(), l.prefix(msg), args...)
}

func (l *logBridge) Warnf(msg string, args ...any) {
	logger.Warnf(context.Background(), l.prefix(msg), args...)
}

func (l *logBridge) Infof(msg string, args ...any) {
	logger.Debugf(context.Background(), l.prefix(msg), args...)
}

func (l *logBridge) Debugf(msg string, args ...any) {}

func (l *logBridge) Sub(module string) waLog.Logger {
	return &logBridge{module: fmt.Sprintf("%s/%s", l.module, module)}
}
