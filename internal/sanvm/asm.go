package sanvm

import (
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/alibertay/san_network/internal/canonical"
)

// AsmError marks a deterministic assembly failure.
type AsmError struct{ Message string }

func (e *AsmError) Error() string { return e.Message }

func asmErrorf(format string, args ...any) *AsmError {
	return &AsmError{Message: fmt.Sprintf(format, args...)}
}

// Instruction is one expanded assembly record.
type Instruction struct {
	Name     string
	Operands []any
}

// LabelRecord is the record name used for label definitions.
const LabelRecord = "__label__"

// MaxLiteralChars bounds literal operands.
const MaxLiteralChars = 65_536

var (
	symbolRegex  = regexp.MustCompile(`^[A-Za-z_$][\w$]*$`)
	labelRegex   = regexp.MustCompile(`^[A-Za-z_.$][\w.$]*$`)
	addressRegex = regexp.MustCompile(`^-?\d+$|^0[xX][0-9a-fA-F]+$|^0[bB][01]+$`)
	funcRegex    = regexp.MustCompile(`(?i)^FUNC\s+([A-Za-z_$][\w$]*)\s*(?:\(([^)]*)\))?\s*\{\s*$`)
	bareAsmRegex = regexp.MustCompile(`(?m)^[ \t]*asm[ \t]*\{`)
	numberRegex  = regexp.MustCompile(
		`^[+-]?(?:0[xX][0-9a-fA-F_]+|0[bB][01_]+|0[oO][0-7_]+|\d[\d_]*(?:\.\d[\d_]*)?(?:[eE][+-]?\d+)?|\.\d[\d_]*(?:[eE][+-]?\d+)?)$`)
)

// StripComment removes a ';' or '//' comment, ignoring string literals.
func StripComment(line string) string {
	var out strings.Builder
	inString := byte(0)
	escape := false
	for i := 0; i < len(line); i++ {
		char := line[i]
		if inString != 0 {
			out.WriteByte(char)
			if escape {
				escape = false
			} else if char == '\\' {
				escape = true
			} else if char == inString {
				inString = 0
			}
			continue
		}
		switch {
		case char == '"' || char == '\'':
			inString = char
			out.WriteByte(char)
		case char == ';':
			return out.String()
		case char == '/' && i+1 < len(line) && line[i+1] == '/':
			return out.String()
		default:
			out.WriteByte(char)
		}
	}
	return out.String()
}

func braceDelta(text string) int {
	delta := 0
	inString := byte(0)
	escape := false
	for i := 0; i < len(text); i++ {
		char := text[i]
		if inString != 0 {
			if escape {
				escape = false
			} else if char == '\\' {
				escape = true
			} else if char == inString {
				inString = 0
			}
			continue
		}
		switch char {
		case '"', '\'':
			inString = char
		case '{':
			delta++
		case '}':
			delta--
		}
	}
	return delta
}

// MatchingBrace returns the index of the brace matching the one at openIndex.
func MatchingBrace(text string, openIndex int) int {
	depth := 0
	inString := byte(0)
	escape := false
	for i := openIndex; i < len(text); i++ {
		char := text[i]
		if inString != 0 {
			if escape {
				escape = false
			} else if char == '\\' {
				escape = true
			} else if char == inString {
				inString = 0
			}
			continue
		}
		switch char {
		case '"', '\'':
			inString = char
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// StripOuterWrapper returns the body of `asm { ... }` when the whole source is
// wrapped.
func StripOuterWrapper(source string) (string, error) {
	match := bareAsmRegex.FindStringIndex(source)
	if match == nil {
		return source, nil
	}
	prefix := source[:match[0]]
	if strings.TrimSpace(StripComment(prefix)) != "" {
		return source, nil
	}
	braceIndex := strings.Index(source[match[0]:], "{") + match[0]
	closeIndex := MatchingBrace(source, braceIndex)
	if closeIndex == -1 {
		return "", asmErrorf("Unterminated asm block")
	}
	tail := strings.TrimSpace(StripComment(source[closeIndex+1:]))
	if tail != "" {
		return "", asmErrorf("Unexpected text after asm block: %q", tail)
	}
	return source[braceIndex+1 : closeIndex], nil
}

// LooksLikeAssembly reports whether the first meaningful line is PASM code.
func LooksLikeAssembly(source string) bool {
	for _, raw := range strings.Split(source, "\n") {
		text := strings.TrimSpace(StripComment(raw))
		if text == "" {
			continue
		}
		if strings.HasPrefix(text, "asm") {
			rest := text[3:]
			if rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '{' {
				return false
			}
		}
		if colon := strings.Index(text, ":"); colon > 0 {
			head := strings.TrimSpace(text[:colon])
			rest := text[colon+1:]
			if !strings.HasPrefix(rest, "=") && labelRegex.MatchString(head) {
				upper := strings.ToUpper(head)
				if _, ok := Mnemonics[upper]; !ok {
					if _, ok := VarMacros[upper]; !ok && upper != "FUNC" {
						return true
					}
				}
				return false
			}
		}
		token := text
		for i := 0; i < len(text); i++ {
			if !(text[i] == '_' || text[i] >= 'A' && text[i] <= 'Z' ||
				text[i] >= 'a' && text[i] <= 'z' || i > 0 && text[i] >= '0' && text[i] <= '9') {
				token = text[:i]
				break
			}
		}
		if token == "" {
			return false
		}
		if _, ok := Mnemonics[token]; ok {
			return true
		}
		if _, ok := VarMacros[token]; ok {
			return true
		}
		return token == "FUNC"
	}
	return false
}

// ---------------------------------------------------------------------- #
// Literal parsing
// ---------------------------------------------------------------------- #

// ParseLiteral parses a PUSH operand: symbol, true/false or Python literal.
func ParseLiteral(text, mnemonic string) (any, error) {
	if text == "" {
		return nil, asmErrorf("%s requires an operand", mnemonic)
	}
	if text == "true" {
		return int64(1), nil
	}
	if text == "false" {
		return int64(0), nil
	}
	if symbolRegex.MatchString(text) {
		return text, nil
	}
	if len(text) > MaxLiteralChars {
		return nil, asmErrorf("%s literal is too large (%d chars)", mnemonic, len(text))
	}
	parser := &literalParser{text: text}
	value, err := parser.parseValue()
	if err != nil {
		return nil, asmErrorf("Invalid literal for %s: %q", mnemonic, text)
	}
	parser.skipSpaces()
	if parser.pos != len(parser.text) {
		return nil, asmErrorf("Invalid literal for %s: %q", mnemonic, text)
	}
	return value, nil
}

// ParseSymbol parses a variable/symbol operand.
func ParseSymbol(text, mnemonic string) (string, error) {
	if text == "" {
		return "", asmErrorf("%s requires a variable name", mnemonic)
	}
	if symbolRegex.MatchString(text) {
		return text, nil
	}
	if len(text) > MaxLiteralChars {
		return "", asmErrorf("%s operand is too large", mnemonic)
	}
	parser := &literalParser{text: text}
	value, err := parser.parseValue()
	if err != nil {
		return "", asmErrorf("Invalid variable name for %s: %q", mnemonic, text)
	}
	text2, ok := value.(string)
	if !ok {
		return "", asmErrorf("%s expects a variable name, got %q", mnemonic, text)
	}
	return text2, nil
}

func parseTarget(text, mnemonic, namespace string) (any, error) {
	if addressRegex.MatchString(text) {
		number, ok := parseIntLiteral(text)
		if !ok {
			return nil, asmErrorf("Invalid address for %s: %q", mnemonic, text)
		}
		return number, nil
	}
	if labelRegex.MatchString(text) {
		return namespace + text, nil
	}
	return nil, asmErrorf("%s expects a label or address, got %q", mnemonic, text)
}

func parseIntLiteral(text string) (int64, bool) {
	cleaned := strings.ReplaceAll(text, "_", "")
	if strings.HasPrefix(cleaned, "0x") || strings.HasPrefix(cleaned, "0X") {
		value, ok := new(big.Int).SetString(cleaned[2:], 16)
		if !ok || !value.IsInt64() {
			return 0, false
		}
		return value.Int64(), true
	}
	if strings.HasPrefix(cleaned, "0b") || strings.HasPrefix(cleaned, "0B") {
		value, ok := new(big.Int).SetString(cleaned[2:], 2)
		if !ok || !value.IsInt64() {
			return 0, false
		}
		return value.Int64(), true
	}
	value, err := strconv.ParseInt(cleaned, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

type literalParser struct {
	text string
	pos  int
}

func (parser *literalParser) skipSpaces() {
	for parser.pos < len(parser.text) && (parser.text[parser.pos] == ' ' ||
		parser.text[parser.pos] == '\t' || parser.text[parser.pos] == '\n' ||
		parser.text[parser.pos] == '\r') {
		parser.pos++
	}
}

func (parser *literalParser) parseValue() (any, error) {
	parser.skipSpaces()
	if parser.pos >= len(parser.text) {
		return nil, fmt.Errorf("unexpected end of literal")
	}
	char := parser.text[parser.pos]
	switch {
	case char == '[':
		return parser.parseList()
	case char == '{':
		return parser.parseDict()
	case char == '"' || char == '\'':
		return parser.parseString()
	case strings.HasPrefix(parser.text[parser.pos:], "True"):
		parser.pos += 4
		return true, nil
	case strings.HasPrefix(parser.text[parser.pos:], "False"):
		parser.pos += 5
		return false, nil
	case strings.HasPrefix(parser.text[parser.pos:], "None"):
		parser.pos += 4
		return nil, nil
	default:
		return parser.parseNumber()
	}
}

func (parser *literalParser) parseList() (any, error) {
	parser.pos++ // '['
	items := []any{}
	for {
		parser.skipSpaces()
		if parser.pos >= len(parser.text) {
			return nil, fmt.Errorf("unterminated list")
		}
		if parser.text[parser.pos] == ']' {
			parser.pos++
			return items, nil
		}
		item, err := parser.parseValue()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		parser.skipSpaces()
		if parser.pos < len(parser.text) && parser.text[parser.pos] == ',' {
			parser.pos++
			continue
		}
		if parser.pos < len(parser.text) && parser.text[parser.pos] == ']' {
			parser.pos++
			return items, nil
		}
		return nil, fmt.Errorf("invalid list literal")
	}
}

func (parser *literalParser) parseDict() (any, error) {
	parser.pos++ // '{'
	result := map[string]any{}
	for {
		parser.skipSpaces()
		if parser.pos >= len(parser.text) {
			return nil, fmt.Errorf("unterminated dict")
		}
		if parser.text[parser.pos] == '}' {
			parser.pos++
			return result, nil
		}
		key, err := parser.parseValue()
		if err != nil {
			return nil, err
		}
		parser.skipSpaces()
		if parser.pos >= len(parser.text) || parser.text[parser.pos] != ':' {
			return nil, fmt.Errorf("invalid dict literal")
		}
		parser.pos++
		value, err := parser.parseValue()
		if err != nil {
			return nil, err
		}
		result[dictKeyString(key)] = value
		parser.skipSpaces()
		if parser.pos < len(parser.text) && parser.text[parser.pos] == ',' {
			parser.pos++
			continue
		}
		if parser.pos < len(parser.text) && parser.text[parser.pos] == '}' {
			parser.pos++
			return result, nil
		}
		return nil, fmt.Errorf("invalid dict literal")
	}
}

func dictKeyString(key any) string {
	switch typed := key.(type) {
	case string:
		return typed
	case int64:
		return strconv.FormatInt(typed, 10)
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case nil:
		return "None"
	default:
		return fmt.Sprintf("%v", typed)
	}
}

func (parser *literalParser) parseString() (string, error) {
	quote := parser.text[parser.pos]
	parser.pos++
	var out strings.Builder
	for parser.pos < len(parser.text) {
		char := parser.text[parser.pos]
		if char == quote {
			parser.pos++
			return out.String(), nil
		}
		if char != '\\' {
			out.WriteByte(char)
			parser.pos++
			continue
		}
		parser.pos++
		if parser.pos >= len(parser.text) {
			break
		}
		escape := parser.text[parser.pos]
		parser.pos++
		switch escape {
		case 'n':
			out.WriteByte('\n')
		case 't':
			out.WriteByte('\t')
		case 'r':
			out.WriteByte('\r')
		case 'b':
			out.WriteByte('\b')
		case 'f':
			out.WriteByte('\f')
		case 'v':
			out.WriteByte('\v')
		case 'a':
			out.WriteByte('\a')
		case '0':
			out.WriteByte(0)
		case '\\', '\'', '"':
			out.WriteByte(escape)
		case 'x':
			hex := parser.takeHex(2)
			if hex < 0 {
				return "", fmt.Errorf("invalid \\x escape")
			}
			out.WriteByte(byte(hex))
		case 'u':
			code := parser.takeHex(4)
			if code < 0 {
				return "", fmt.Errorf("invalid \\u escape")
			}
			out.WriteRune(rune(code))
		case 'U':
			code := parser.takeHex(8)
			if code < 0 {
				return "", fmt.Errorf("invalid \\U escape")
			}
			out.WriteRune(rune(code))
		default:
			out.WriteByte('\\')
			out.WriteByte(escape)
		}
	}
	return "", fmt.Errorf("unterminated string")
}

func (parser *literalParser) takeHex(count int) int64 {
	if parser.pos+count > len(parser.text) {
		return -1
	}
	text := parser.text[parser.pos : parser.pos+count]
	value, err := strconv.ParseInt(text, 16, 64)
	if err != nil {
		return -1
	}
	parser.pos += count
	return value
}

func (parser *literalParser) parseNumber() (any, error) {
	start := parser.pos
	for parser.pos < len(parser.text) {
		char := parser.text[parser.pos]
		if char == ' ' || char == '\t' || char == ',' || char == ']' || char == '}' ||
			char == ':' || char == '\n' || char == '\r' {
			break
		}
		parser.pos++
	}
	token := parser.text[start:parser.pos]
	if !numberRegex.MatchString(token) {
		return nil, fmt.Errorf("invalid number")
	}
	cleaned := strings.ReplaceAll(token, "_", "")
	integerPart := strings.HasPrefix(cleaned, "0x") || strings.HasPrefix(cleaned, "0X") ||
		strings.HasPrefix(cleaned, "0b") || strings.HasPrefix(cleaned, "0B") ||
		strings.HasPrefix(cleaned, "0o") || strings.HasPrefix(cleaned, "0O")
	if integerPart {
		base := 10
		digits := cleaned
		negative := strings.HasPrefix(digits, "-")
		positive := strings.HasPrefix(digits, "+")
		if negative || positive {
			digits = digits[1:]
		}
		switch {
		case strings.HasPrefix(digits, "0x") || strings.HasPrefix(digits, "0X"):
			base = 16
			digits = digits[2:]
		case strings.HasPrefix(digits, "0b") || strings.HasPrefix(digits, "0B"):
			base = 2
			digits = digits[2:]
		case strings.HasPrefix(digits, "0o") || strings.HasPrefix(digits, "0O"):
			base = 8
			digits = digits[2:]
		}
		value, ok := new(big.Int).SetString(digits, base)
		if !ok {
			return nil, fmt.Errorf("invalid integer")
		}
		if negative {
			value.Neg(value)
		}
		return normalizeInt(value), nil
	}
	if strings.ContainsAny(cleaned, ".eE") {
		floatValue, err := strconv.ParseFloat(cleaned, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid float")
		}
		return floatValue, nil
	}
	value, ok := new(big.Int).SetString(cleaned, 10)
	if !ok {
		return nil, fmt.Errorf("invalid integer")
	}
	return normalizeInt(value), nil
}

// ---------------------------------------------------------------------- #
// Assembler
// ---------------------------------------------------------------------- #

type assembler struct {
	namespace    string
	instructions []Instruction
	funcCounter  int
}

// ParseLines expands an assembly program into instruction records.
func ParseLines(lines []string, namespace string) ([]Instruction, error) {
	builder := &assembler{namespace: namespace, instructions: []Instruction{}}
	if err := builder.parseLines(lines); err != nil {
		return nil, err
	}
	return builder.instructions, nil
}

// ParseSource expands assembly source into instruction records.
func ParseSource(source, namespace string) ([]Instruction, error) {
	return ParseLines(strings.Split(source, "\n"), namespace)
}

func (builder *assembler) emit(name string, operands ...any) {
	builder.instructions = append(builder.instructions, Instruction{Name: name, Operands: operands})
}

func (builder *assembler) label(name string) {
	builder.emit(LabelRecord, name)
}

func (builder *assembler) parseLines(lines []string) error {
	i := 0
	for i < len(lines) {
		text := strings.TrimSpace(StripComment(lines[i]))
		i++
		if text == "" {
			continue
		}
		if text == "}" {
			return asmErrorf("Unexpected '}': not inside a FUNC block")
		}

		if match := funcRegex.FindStringSubmatch(text); match != nil {
			body := []string{}
			depth := 1
			closed := false
			for i < len(lines) {
				raw := StripComment(lines[i])
				depth += braceDelta(raw)
				i++
				if depth <= 0 {
					closed = true
					break
				}
				body = append(body, raw)
			}
			if !closed {
				return asmErrorf("Unterminated FUNC block: %q", text)
			}
			if err := builder.emitFunction(match[1], match[2], body); err != nil {
				return err
			}
			continue
		}

		label, rest, isLabel := splitLabel(text)
		if isLabel {
			builder.label(builder.namespace + label)
			if rest != "" {
				if err := builder.compile(rest); err != nil {
					return err
				}
			}
			continue
		}

		if err := builder.compile(text); err != nil {
			return err
		}
	}
	return nil
}

func splitLabel(text string) (string, string, bool) {
	colon := strings.Index(text, ":")
	if colon < 0 {
		return "", text, false
	}
	head := strings.TrimSpace(text[:colon])
	rest := strings.TrimSpace(text[colon+1:])
	if head == "" || !labelRegex.MatchString(head) {
		return "", text, false
	}
	if strings.HasPrefix(rest, "=") {
		return "", text, false
	}
	upper := strings.ToUpper(head)
	if _, ok := Mnemonics[upper]; ok {
		return "", text, false
	}
	if _, ok := VarMacros[upper]; ok || upper == "FUNC" {
		return "", text, false
	}
	return head, rest, true
}

func (builder *assembler) compile(text string) error {
	parts := strings.Fields(text)
	mnemonic := strings.ToUpper(parts[0])
	operand := ""
	if len(parts) > 1 {
		operand = strings.TrimSpace(text[strings.Index(text, parts[0])+len(parts[0]):])
	}

	switch {
	case mnemonic == "PUSH":
		value, err := ParseLiteral(operand, "PUSH")
		if err != nil {
			return err
		}
		builder.emit("PUSH", value)
		return nil

	case TargetMnemonics[mnemonic]:
		target, err := parseTarget(operand, mnemonic, builder.namespace)
		if err != nil {
			return err
		}
		builder.emit(mnemonic, target)
		return nil

	case hasVarMacro(mnemonic):
		if operand != "" {
			symbol, err := ParseSymbol(operand, mnemonic)
			if err != nil {
				return err
			}
			builder.emit("PUSH", symbol)
			arity := VarMacros[mnemonic]
			if arity == 2 {
				builder.emit("SWAP")
			} else if arity == 3 {
				builder.emit("ROT")
				builder.emit("ROT")
			}
			builder.emit(mnemonic)
			return nil
		}
	}

	if _, ok := Mnemonics[mnemonic]; ok {
		if operand != "" {
			return asmErrorf("%s does not take operands: %q", mnemonic, text)
		}
		builder.emit(mnemonic)
		return nil
	}
	return asmErrorf("Unknown mnemonic: %q", parts[0])
}

func hasVarMacro(mnemonic string) bool {
	_, ok := VarMacros[mnemonic]
	return ok
}

func (builder *assembler) emitFunction(name, paramsRaw string, body []string) error {
	params := []string{}
	for _, param := range strings.Split(paramsRaw, ",") {
		param = strings.TrimSpace(param)
		if param == "" {
			continue
		}
		if !symbolRegex.MatchString(param) {
			return asmErrorf("Invalid parameter name: %q", param)
		}
		params = append(params, param)
	}

	builder.funcCounter++
	bodyLabel := fmt.Sprintf("%s__func_%d__body", builder.namespace, builder.funcCounter)
	skipLabel := fmt.Sprintf("%s__func_%d__skip", builder.namespace, builder.funcCounter)

	builder.emit("PUSH", name)
	for _, param := range params {
		builder.emit("PUSH", param)
	}
	builder.emit("PUSH", int64(len(params)))
	builder.emit("DEF_FUNC", bodyLabel)
	builder.emit("JMP", skipLabel)
	builder.label(bodyLabel)
	if err := builder.parseLines(body); err != nil {
		return err
	}
	builder.emit("END_FUNC")
	builder.label(skipLabel)
	return nil
}

func instructionSize(name string) int {
	if name == "PUSH" || TargetMnemonics[name] {
		return 2
	}
	return 1
}

// Encode resolves labels and encodes assembly records into bytecode.
func Encode(instructions []Instruction) ([]any, error) {
	labels := map[string]int64{}
	position := int64(0)
	for _, instruction := range instructions {
		if instruction.Name == LabelRecord {
			if len(instruction.Operands) == 0 {
				return nil, asmErrorf("Label record without a name")
			}
			name, ok := instruction.Operands[0].(string)
			if !ok {
				return nil, asmErrorf("Invalid label record: %v", instruction.Operands[0])
			}
			if _, exists := labels[name]; exists {
				return nil, asmErrorf("Duplicate label: %s", name)
			}
			labels[name] = position
			continue
		}
		if _, ok := Mnemonics[instruction.Name]; !ok {
			return nil, asmErrorf("Unknown mnemonic: %q", instruction.Name)
		}
		position += int64(instructionSize(instruction.Name))
	}
	total := position

	bytecode := []any{}
	for _, instruction := range instructions {
		if instruction.Name == LabelRecord {
			continue
		}
		bytecode = append(bytecode, int(Mnemonics[instruction.Name]))
		switch {
		case instruction.Name == "PUSH":
			if len(instruction.Operands) == 0 {
				return nil, asmErrorf("PUSH without an operand")
			}
			bytecode = append(bytecode, instruction.Operands[0])
		case TargetMnemonics[instruction.Name]:
			if len(instruction.Operands) == 0 {
				return nil, asmErrorf("%s without a target", instruction.Name)
			}
			target := instruction.Operands[0]
			switch typed := target.(type) {
			case string:
				resolved, ok := labels[typed]
				if !ok {
					return nil, asmErrorf("Unknown label: %s", typed)
				}
				bytecode = append(bytecode, resolved)
			case int64:
				if typed < 0 || typed > total {
					return nil, asmErrorf("Target out of range: %d", typed)
				}
				bytecode = append(bytecode, typed)
			case int:
				if int64(typed) < 0 || int64(typed) > total {
					return nil, asmErrorf("Target out of range: %d", typed)
				}
				bytecode = append(bytecode, int64(typed))
			default:
				return nil, asmErrorf("Invalid target: %v", target)
			}
		}
	}
	return bytecode, nil
}

// AssembleLines assembles expanded lines.
func AssembleLines(lines []string, namespace string) ([]any, error) {
	instructions, err := ParseLines(lines, namespace)
	if err != nil {
		return nil, err
	}
	return Encode(instructions)
}

// Assemble compiles assembly source (optionally `asm { ... }` wrapped).
func Assemble(source string) ([]any, error) {
	body, err := StripOuterWrapper(source)
	if err != nil {
		return nil, err
	}
	return AssembleLines(strings.Split(body, "\n"), "")
}

// ---------------------------------------------------------------------- #
// Disassembler
// ---------------------------------------------------------------------- #

// FormatLiteral renders a PUSH operand as PASM text.
func FormatLiteral(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case string:
		return strconv.Quote(typed)
	case []any:
		parts := make([]string, len(typed))
		for i, item := range typed {
			parts[i] = FormatLiteral(item)
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
			parts = append(parts, strconv.Quote(key)+": "+FormatLiteral(typed[key]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		text, err := canonical.MarshalString(value)
		if err != nil {
			return fmt.Sprintf("%v", value)
		}
		return text
	}
}

// Disassemble renders bytecode as PASM text.
func Disassemble(bytecode []any) (string, error) {
	items := bytecode
	targets := map[int64]bool{}
	i := 0
	for i < len(items) {
		opcode, ok := opcodeInt(items[i])
		if !ok {
			i++
			continue
		}
		name, known := OpNames[opcode]
		if !known {
			i++
			continue
		}
		if name == "PUSH" {
			i += 2
			continue
		}
		if TargetMnemonics[name] {
			if i+1 >= len(items) {
				return "", asmErrorf("Truncated %s instruction at position %d", name, i)
			}
			target, ok := opcodeInt(items[i+1])
			if !ok {
				return "", asmErrorf("Truncated %s instruction at position %d", name, i)
			}
			targets[int64(target)] = true
			i += 2
			continue
		}
		i++
	}

	lines := []string{}
	i = 0
	for i < len(items) {
		if targets[int64(i)] {
			lines = append(lines, fmt.Sprintf(".L%d:", i))
		}
		opcode, ok := opcodeInt(items[i])
		if !ok {
			return "", asmErrorf("Invalid opcode at position %d: %v", i, items[i])
		}
		name, known := OpNames[opcode]
		if !known {
			return "", asmErrorf("Invalid opcode at position %d: %v", i, items[i])
		}
		if name == "PUSH" {
			if i+1 >= len(items) {
				return "", asmErrorf("Truncated PUSH instruction at position %d", i)
			}
			lines = append(lines, "PUSH "+FormatLiteral(items[i+1]))
			i += 2
			continue
		}
		if TargetMnemonics[name] {
			if i+1 >= len(items) {
				return "", asmErrorf("Truncated %s instruction at position %d", name, i)
			}
			target, _ := opcodeInt(items[i+1])
			lines = append(lines, fmt.Sprintf("%s .L%d", name, target))
			i += 2
			continue
		}
		lines = append(lines, name)
		i++
	}
	if targets[int64(len(items))] {
		lines = append(lines, fmt.Sprintf(".L%d:", len(items)))
	}
	return strings.Join(lines, "\n"), nil
}
