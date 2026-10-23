package util

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func TextOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, it := range x {
			if mm, ok := it.(map[string]any); ok {
				if t := Str(mm["text"]); t != "" {
					parts = append(parts, t)
				} else {
					b, _ := json.Marshal(mm)
					parts = append(parts, string(b))
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func SummaryText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	arr, _ := v.([]any)
	var parts []string
	for _, it := range arr {
		if m, ok := it.(map[string]any); ok {
			if t := Str(m["text"]); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func Str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return fmt.Sprintf("%v", x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func Number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}
