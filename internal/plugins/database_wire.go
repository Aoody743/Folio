package plugins

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

var decimalSQLInteger = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,19})$`)

func exactSQLInteger(value any) any {
	switch n := value.(type) {
	case int64:
		if n > int64(maxSafeJSONInteger) || n < -int64(maxSafeJSONInteger) {
			return map[string]any{"encoding": "integer", "value": strconv.FormatInt(n, 10)}
		}
	case uint64:
		if n > maxSafeJSONInteger {
			return map[string]any{"encoding": "integer", "value": strconv.FormatUint(n, 10)}
		}
	}
	return value
}

func integerColumnText(kind, value string) any {
	switch strings.ToUpper(kind) {
	case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER", "BIGINT", "UNSIGNED TINYINT", "UNSIGNED SMALLINT", "UNSIGNED MEDIUMINT", "UNSIGNED INT", "UNSIGNED BIGINT", "INT2", "INT4", "INT8":
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			if n > int64(maxSafeJSONInteger) || n < -int64(maxSafeJSONInteger) {
				return exactSQLInteger(n)
			}
		} else if n, err := strconv.ParseUint(value, 10, 64); err == nil && n > maxSafeJSONInteger {
			return exactSQLInteger(n)
		}
	}
	return value
}

func exactSQLParameter(value any) (any, error) {
	if number, ok := value.(json.Number); ok {
		// The drivers infer the selected SQL parameter's type. Bind exact decimal
		// text instead of converting it to float64 and silently rounding it.
		if len(number.String()) > 128 {
			return nil, failure("invalid_input", "SQL numeric parameter exceeds bounds", 400)
		}
		return number.String(), nil
	}
	if object, ok := value.(map[string]any); ok {
		encoding, _ := object["encoding"].(string)
		text, _ := object["value"].(string)
		if len(object) != 2 || encoding != "integer" || !decimalSQLInteger.MatchString(text) {
			return nil, failure("invalid_input", "Structured SQL parameters must use exact integer encoding", 400)
		}
		if _, err := strconv.ParseInt(text, 10, 64); err != nil {
			if _, err := strconv.ParseUint(text, 10, 64); err != nil {
				return nil, failure("invalid_input", "SQL integer parameter exceeds 64-bit bounds", 400)
			}
		}
		return text, nil
	}
	return value, nil
}
