package im

import (
	"encoding/json"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// ParseCredentials parses the JSONB credentials field into a map.
func ParseCredentials(data []byte) (map[string]any, error) {
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var creds map[string]any
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, err
	}
	return creds, nil
}

// GetString safely extracts a string value from a credentials map.
func GetString(creds map[string]any, key string) string {
	if v, ok := creds[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// MergeUpdatedCredentials decides what an update request should persist as
// the channel credentials. List responses redact credentials, so an edit
// form may legitimately send back an empty object — treat that as "not
// provided" and keep the stored value instead of wiping it. For WhatsApp,
// device_jid is additionally inherited from the stored credentials when the
// update omits it, so editing e.g. allow_from can never unbind the device.
func MergeUpdatedCredentials(platform string, oldCred, newCred types.JSON) types.JSON {
	s := strings.TrimSpace(string(newCred))
	if s == "" || s == "{}" || s == "null" {
		return oldCred
	}
	if platform != string(PlatformWhatsApp) {
		return newCred
	}
	newMap, err := ParseCredentials(newCred)
	if err != nil || newMap == nil || GetString(newMap, "device_jid") != "" {
		return newCred
	}
	oldMap, err := ParseCredentials(oldCred)
	if err != nil {
		return newCred
	}
	if jid := GetString(oldMap, "device_jid"); jid != "" {
		newMap["device_jid"] = jid
		if merged, err := json.Marshal(newMap); err == nil {
			return types.JSON(merged)
		}
	}
	return newCred
}

// GetBool reads a boolean from JSON credentials (bool, string "true"/"1", or non-zero number).
func GetBool(creds map[string]any, key string) bool {
	v, ok := creds[key]
	if !ok {
		return false
	}
	switch x := v.(type) {
	case bool:
		return x
	case string:
		s := strings.TrimSpace(strings.ToLower(x))
		return s == "true" || s == "1" || s == "yes"
	case float64:
		return x != 0
	case int:
		return x != 0
	default:
		return false
	}
}
