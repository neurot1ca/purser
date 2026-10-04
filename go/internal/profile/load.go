package profile

import (
	"bytes"

	"github.com/BurntSushi/toml"
)

// DecodeTOML returns the generic table shape consumed by the profile tier validators.
func DecodeTOML(raw []byte) (map[string]any, error) {
	data := map[string]any{}
	_, err := toml.NewDecoder(bytes.NewReader(raw)).Decode(&data)
	if err != nil {
		return nil, err
	}
	return normalizeDecodedValue(data).(map[string]any), nil
}

func normalizeDecodedValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			value[key] = normalizeDecodedValue(item)
		}
		return value
	case []map[string]any:
		items := make([]any, len(value))
		for i, item := range value {
			items[i] = normalizeDecodedValue(item)
		}
		return items
	case []any:
		for i, item := range value {
			value[i] = normalizeDecodedValue(item)
		}
		return value
	default:
		return value
	}
}
