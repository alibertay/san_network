package sdk

import (
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/alibertay/san_network/internal/canonical"
)

// PythonRepr renders a decoded JSON value the way Python's repr() does; it is
// used for error strings (sdk/client.py interpolates response bodies) and for
// run_node status messages.
func PythonRepr(value any) string { return pythonRepr(value) }

// pythonRepr renders a decoded JSON value the way Python's repr() does for
// the error strings SanClientError carries (sdk/client.py interpolates the
// response body dict directly).
func pythonRepr(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case string:
		return pythonReprString(typed)
	case int:
		return strconv.FormatInt(int64(typed), 10)
	case int8:
		return strconv.FormatInt(int64(typed), 10)
	case int16:
		return strconv.FormatInt(int64(typed), 10)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case int64:
		return strconv.FormatInt(typed, 10)
	case uint:
		return strconv.FormatUint(uint64(typed), 10)
	case uint64:
		return strconv.FormatUint(typed, 10)
	case float32:
		return pythonFloatRepr(float64(typed))
	case float64:
		return pythonFloatRepr(typed)
	case *big.Int:
		if typed == nil {
			return "None"
		}
		return typed.String()
	case []any:
		parts := make([]string, len(typed))
		for i, item := range typed {
			parts[i] = pythonRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []map[string]any:
		parts := make([]string, len(typed))
		for i, item := range typed {
			parts[i] = pythonRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		parts := make([]string, len(typed))
		for i, item := range typed {
			parts[i] = pythonReprString(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, pythonReprString(key)+": "+pythonRepr(typed[key]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprintf("%v", value)
	}
}

func pythonFloatRepr(value float64) string {
	text, err := canonical.MarshalString(value)
	if err != nil {
		return strconv.FormatFloat(value, 'g', -1, 64)
	}
	return text
}

func pythonReprString(value string) string {
	quote := byte('\'')
	if strings.Contains(value, "'") && !strings.Contains(value, "\"") {
		quote = '"'
	}
	var builder strings.Builder
	builder.WriteByte(quote)
	for _, r := range value {
		switch r {
		case '\\':
			builder.WriteString(`\\`)
		case '\'':
			if quote == '\'' {
				builder.WriteString(`\'`)
			} else {
				builder.WriteRune(r)
			}
		case '"':
			if quote == '"' {
				builder.WriteString(`\"`)
			} else {
				builder.WriteRune(r)
			}
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&builder, `\x%02x`, r)
			} else {
				builder.WriteRune(r)
			}
		}
	}
	builder.WriteByte(quote)
	return builder.String()
}
