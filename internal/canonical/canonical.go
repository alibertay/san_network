// Package canonical mirrors Python's
//
//	json.dumps(payload, sort_keys=True, separators=(",", ":"))
//
// byte for byte. Every signature, hash and persistence snapshot must
// serialize identically on every node, so all call sites go through these
// helpers.
package canonical

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Marshal returns the canonical JSON encoding of v.
func Marshal(v any) ([]byte, error) {
	var b strings.Builder
	if err := appendValue(&b, reflect.ValueOf(v)); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// MarshalString returns the canonical JSON encoding as a string.
func MarshalString(v any) (string, error) {
	data, err := Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Dumps is the Python-style helper name.
func Dumps(v any) (string, error) { return MarshalString(v) }

// Decode parses JSON like Python's json.loads: integers stay integers (int64
// when they fit, *big.Int otherwise) and floats become float64.
func Decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("invalid JSON: trailing data")
	}
	return Normalize(v), nil
}

// Normalize converts json.Number values into int64 / *big.Int / float64 and
// recurses into arrays and objects.
func Normalize(v any) any {
	switch value := v.(type) {
	case json.Number:
		return normalizeNumber(value)
	case []any:
		for i, item := range value {
			value[i] = Normalize(item)
		}
		return value
	case map[string]any:
		for key, item := range value {
			value[key] = Normalize(item)
		}
		return value
	default:
		return v
	}
}

func normalizeNumber(number json.Number) any {
	text := number.String()
	if !strings.ContainsAny(text, ".eE") {
		if integer, err := number.Int64(); err == nil {
			return integer
		}
		if bigValue, ok := new(big.Int).SetString(text, 10); ok {
			return bigValue
		}
	}
	if floatValue, err := number.Float64(); err == nil {
		return floatValue
	}
	return text
}

// ---------------------------------------------------------------------- #
// Encoder
// ---------------------------------------------------------------------- #

func appendValue(b *strings.Builder, value reflect.Value) error {
	if !value.IsValid() {
		b.WriteString("null")
		return nil
	}

	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			b.WriteString("null")
			return nil
		}
		return appendValue(b, value.Elem())
	}

	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			b.WriteString("null")
			return nil
		}
		if bigValue, ok := value.Interface().(*big.Int); ok {
			if bigValue == nil {
				b.WriteString("null")
				return nil
			}
			b.WriteString(bigValue.String())
			return nil
		}
		return appendValue(b, value.Elem())
	}

	if value.CanInterface() {
		if number, ok := value.Interface().(json.Number); ok {
			return appendJSONNumber(b, number)
		}
	}

	switch value.Kind() {
	case reflect.Bool:
		if value.Bool() {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString(strconv.FormatInt(value.Int(), 10))
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		b.WriteString(strconv.FormatUint(value.Uint(), 10))
		return nil
	case reflect.Float32:
		appendFloat(b, value.Convert(reflect.TypeOf(float64(0))).Float())
		return nil
	case reflect.Float64:
		appendFloat(b, value.Float())
		return nil
	case reflect.String:
		appendString(b, value.String())
		return nil
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			b.WriteString("null")
			return nil
		}
		if value.Type().Elem().Kind() == reflect.Uint8 && value.Kind() == reflect.Slice {
			if _, ok := value.Interface().([]byte); ok {
				return fmt.Errorf("canonical: bytes are not JSON serializable")
			}
		}
		b.WriteByte('[')
		for i := 0; i < value.Len(); i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := appendValue(b, value.Index(i)); err != nil {
				return err
			}
		}
		b.WriteByte(']')
		return nil
	case reflect.Map:
		if value.IsNil() {
			b.WriteString("null")
			return nil
		}
		if value.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("canonical: only string map keys are supported, got %s", value.Type())
		}
		keys := value.MapKeys()
		names := make([]string, len(keys))
		for i, key := range keys {
			names[i] = key.String()
		}
		sort.Strings(names)
		b.WriteByte('{')
		for i, name := range names {
			if i > 0 {
				b.WriteByte(',')
			}
			appendString(b, name)
			b.WriteByte(':')
			if err := appendValue(b, value.MapIndex(reflect.ValueOf(name).Convert(value.Type().Key()))); err != nil {
				return err
			}
		}
		b.WriteByte('}')
		return nil
	case reflect.Invalid:
		b.WriteString("null")
		return nil
	}

	if value.Type() == reflect.TypeOf(big.Int{}) {
		bigValue := value.Interface().(big.Int)
		b.WriteString(bigValue.String())
		return nil
	}

	return fmt.Errorf("canonical: unsupported type %s", value.Type())
}

func appendJSONNumber(b *strings.Builder, number json.Number) error {
	text := number.String()
	if !strings.ContainsAny(text, ".eE") {
		b.WriteString(text)
		return nil
	}
	floatValue, err := number.Float64()
	if err != nil {
		return fmt.Errorf("canonical: invalid number %q", text)
	}
	appendFloat(b, floatValue)
	return nil
}

// appendFloat reproduces Python's repr()/str() formatting for float64.
func appendFloat(b *strings.Builder, value float64) {
	if math.IsNaN(value) {
		b.WriteString("NaN")
		return
	}
	if math.IsInf(value, 1) {
		b.WriteString("Infinity")
		return
	}
	if math.IsInf(value, -1) {
		b.WriteString("-Infinity")
		return
	}
	if value == 0 {
		if math.Signbit(value) {
			b.WriteString("-0.0")
		} else {
			b.WriteString("0.0")
		}
		return
	}

	text := strconv.FormatFloat(value, 'e', -1, 64)
	negative := strings.HasPrefix(text, "-")
	if negative {
		text = text[1:]
	}
	mantissa, exponentText, _ := strings.Cut(text, "e")
	exponent, _ := strconv.Atoi(exponentText)
	digits := strings.Replace(mantissa, ".", "", 1)

	var out string
	switch {
	case exponent < -4 || exponent >= 16:
		if len(digits) == 1 {
			out = digits
		} else {
			out = digits[:1] + "." + digits[1:]
		}
		out += "e" + formatExponent(exponent)
	case exponent >= 0:
		if len(digits) > exponent+1 {
			out = digits[:exponent+1] + "." + digits[exponent+1:]
		} else {
			out = digits + strings.Repeat("0", exponent+1-len(digits)) + ".0"
		}
	default:
		out = "0." + strings.Repeat("0", -exponent-1) + digits
	}
	if negative {
		out = "-" + out
	}
	b.WriteString(out)
}

func formatExponent(exponent int) string {
	sign := "+"
	if exponent < 0 {
		sign = "-"
		exponent = -exponent
	}
	text := strconv.Itoa(exponent)
	if len(text) < 2 {
		text = "0" + text
	}
	return sign + text
}

func appendString(b *strings.Builder, value string) {
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r > 0x7e {
				if r > 0xffff {
					high, low := utf16.EncodeRune(r)
					fmt.Fprintf(b, `\u%04x\u%04x`, high, low)
				} else {
					fmt.Fprintf(b, `\u%04x`, r)
				}
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}
