package plugins

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

const maxSafeJSONInteger = uint64(1<<53 - 1)

var decimalCursor = regexp.MustCompile(`^(0|[1-9][0-9]{0,19})$`)

type cacheCursor uint64

func (c *cacheCursor) UnmarshalJSON(raw []byte) error {
	value := string(raw)
	quoted := len(raw) > 0 && raw[0] == '"'
	if quoted {
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
	}
	if !decimalCursor.MatchString(value) {
		return fmt.Errorf("cursor must be an unsigned decimal integer string")
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || !quoted && n > maxSafeJSONInteger {
		return fmt.Errorf("cursor exceeds exact JSON numeric bounds; use a decimal string")
	}
	*c = cacheCursor(n)
	return nil
}

func (c cacheCursor) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatUint(uint64(c), 10))
}

func cacheCursorSchema() map[string]any {
	return map[string]any{"description": "Exact uint64 continuation cursor; responses use decimal strings. Legacy safe JSON integers remain accepted.", "oneOf": []any{
		map[string]any{"type": "string", "pattern": decimalCursor.String(), "maxLength": 20},
		map[string]any{"type": "integer", "minimum": 0, "maximum": maxSafeJSONInteger},
	}}
}
