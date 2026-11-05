package store

import "encoding/json"

// scanJSONB decodes a jsonb column that was scanned into raw. SQL NULL (nil
// raw) and JSON null leave v untouched and report false, so every reader
// treats a missing document the same way.
func scanJSONB(raw []byte, v any) (bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return false, nil
	}
	return true, json.Unmarshal(raw, v)
}
