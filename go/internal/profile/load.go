package profile

import (
	"bytes"

	"github.com/BurntSushi/toml"
)

// DecodeTOML returns the generic table and source-ordered key paths used by tier validation.
func DecodeTOML(raw []byte) (map[string]any, []toml.Key, error) {
	data := map[string]any{}
	metadata, err := toml.NewDecoder(bytes.NewReader(raw)).Decode(&data)
	if err != nil {
		return nil, nil, err
	}
	return normalizeDecodedValue(data).(map[string]any), metadata.Keys(), nil
}

// ValidateTierTOML decodes and validates a raw profile while preserving source order.
func ValidateTierTOML(tier string, raw []byte) Tier {
	data, keys, err := DecodeTOML(raw)
	if err != nil {
		return Tier{Status: "rejected", Values: map[string]any{}, Warnings: []string{
			tier + " tier rejected: could not parse profile: " + err.Error(),
		}}
	}
	return ValidateTier(tier, data, keys)
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
