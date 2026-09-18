package canonical

import (
	"math"
	"math/big"
	"testing"
)

// guardNoPanic fails the test when fn panics.
func guardNoPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("%s panicked: %v", name, recovered)
		}
	}()
	fn()
}

func TestDecodeRejectsTrailingData(t *testing.T) {
	for _, input := range []string{`{} {}`, `1 2`, `[1]]`, `"a" "b"`} {
		if _, err := Decode([]byte(input)); err == nil {
			t.Errorf("Decode(%q) accepted trailing data", input)
		}
	}
}

func TestDecodeNormalizesNumbers(t *testing.T) {
	decoded, err := Decode([]byte(`{"small":42,"big":123456789012345678901234567890,"float":3.0}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	object := decoded.(map[string]any)
	if _, ok := object["small"].(int64); !ok {
		t.Errorf("small int: got %T", object["small"])
	}
	if _, ok := object["big"].(*big.Int); !ok {
		t.Errorf("big int: got %T", object["big"])
	}
	if _, ok := object["float"].(float64); !ok {
		t.Errorf("float: got %T", object["float"])
	}
}

func TestDecodeNeverPanicsOnHostileInput(t *testing.T) {
	inputs := []string{
		"",
		"{",
		"[",
		"{\"a\":}",
		"{\"a\":1,\"a\":2}",
		"null",
		"true",
		"1e999999",
		"-0.0",
		"{\"nested\":" + repeat("[", 200) + repeat("]", 200) + "}",
		"\x00\xff\xfe",
		stringsOfLength(1<<16, 'x'),
	}
	for _, input := range inputs {
		guardNoPanic(t, "Decode", func() {
			value, err := Decode([]byte(input))
			if err == nil {
				guardNoPanic(t, "Marshal(decoded)", func() {
					_, _ = Marshal(value)
				})
			}
		})
	}
}

func TestMarshalSpecialFloats(t *testing.T) {
	cases := []struct {
		value    float64
		expected string
	}{
		{math.NaN(), "NaN"},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
		{math.Copysign(0, -1), "-0.0"},
		{0, "0.0"},
		{3.0, "3.0"},
		{1e20, "1e+20"},
		{float64(1) / float64(10), "0.1"},
	}
	for _, testCase := range cases {
		encoded, err := Marshal(testCase.value)
		if err != nil {
			t.Fatalf("Marshal(%v): %v", testCase.value, err)
		}
		if string(encoded) != testCase.expected {
			t.Errorf("Marshal(%v) = %q, want %q", testCase.value, encoded, testCase.expected)
		}
	}
}

func repeat(text string, count int) string {
	out := ""
	for i := 0; i < count; i++ {
		out += text
	}
	return out
}

func stringsOfLength(length int, char byte) string {
	buffer := make([]byte, length)
	for i := range buffer {
		buffer[i] = char
	}
	return string(buffer)
}

func FuzzDecode(f *testing.F) {
	seeds := []string{
		`{}`, `[]`, `null`, `{"a":[1,2,3]}`, `{"n":1e309}`, `{"big":99999999999999999999999999}`,
		`{"s":"\ud800"}`, `{"x":-0.0}`, `{`, `[1,`, `"\xff"`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		value, err := Decode(data)
		if err != nil {
			return
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("Marshal(Decode(%q)) panicked: %v", data, recovered)
			}
		}()
		_, _ = Marshal(value)
	})
}
