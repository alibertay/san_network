package sanvm

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// PenaParser lowers high-level PENA to PENA Assembly and then to bytecode,
// mirroring SANVM/pena_parser.py.

var (
	tokenRegex     = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|\d+\.\d+|\d+|[A-Za-z_]\w*|==|!=|<=|>=|&&|\|\||[-+*/%<>()\[\],!]`)
	functionRegex  = regexp.MustCompile(`^function\s+(\w+)\s*\((.*?)\)`)
	forRegex       = regexp.MustCompile(`^for\s+(\w+)\s*,\s*(-?\d+)\s*->\s*(-?\d+)`)
	subscriptRegex = regexp.MustCompile(`^(\w+)\s*\[(.*)\]$`)
	callRegex      = regexp.MustCompile(`(\w+)\s*\((.*)\)`)
	conditionRegex = regexp.MustCompile(`\((.*)\)`)
	numberRegex2   = regexp.MustCompile(`^\d+(\.\d+)?$`)
	asmBlockRegex  = regexp.MustCompile(`(?m)^[ \t]*asm[ \t]*\{`)
)

var binaryMnemonics = map[string]string{
	"+": "ADD", "-": "SUB", "*": "MUL", "/": "DIV", "%": "MOD",
	"==": "EQ", "!=": "NEQ", "<": "LT", "<=": "LTE", ">": "GT", ">=": "GTE",
}

var comparisonOperators = map[string]bool{
	"==": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true,
}

// PenaParser is the high-level PENA compiler state.
type PenaParser struct {
	program      []Instruction
	labelCounter int
	loopStack    [][2]string
	tokens       []string
	position     int
	asmBlocks    map[int]string
}

// NewPenaParser creates an empty compiler.
func NewPenaParser() *PenaParser {
	return &PenaParser{}
}

// Parse compiles PENA source into bytecode.
func (parser *PenaParser) Parse(source string) ([]any, error) {
	cleaned, blocks, err := parser.extractAsmBlocks(source)
	if err != nil {
		return nil, err
	}
	parser.asmBlocks = blocks
	parser.program = []Instruction{}
	parser.labelCounter = 0
	parser.loopStack = [][2]string{}

	lines := parser.preprocess(cleaned)
	if err := parser.parseBlock(lines, 0); err != nil {
		return nil, err
	}
	return Encode(parser.program)
}

// CompilePena compiles PENA or PENA Assembly source into bytecode.
func CompilePena(source, language string) ([]any, error) {
	selected := language
	if selected == "" {
		if LooksLikeAssembly(source) {
			selected = "asm"
		} else {
			selected = "pena"
		}
	} else {
		selected = strings.ToLower(strings.TrimSpace(selected))
	}
	switch selected {
	case "asm", "assembly", "pasm":
		return Assemble(source)
	case "pena", "hl", "high-level":
		return NewPenaParser().Parse(source)
	default:
		return nil, fmt.Errorf("Unknown language: %q", language)
	}
}

// ---------------------------------------------------------------------- #
// Emission helpers
// ---------------------------------------------------------------------- #

func (parser *PenaParser) emit(mnemonic string, operands ...any) {
	parser.program = append(parser.program, Instruction{Name: mnemonic, Operands: operands})
}

func (parser *PenaParser) label(name string) {
	parser.emit(LabelRecord, name)
}

func (parser *PenaParser) newLabel() string {
	parser.labelCounter++
	return fmt.Sprintf(".L%d", parser.labelCounter)
}

// ---------------------------------------------------------------------- #
// Inline assembly blocks
// ---------------------------------------------------------------------- #

func (parser *PenaParser) extractAsmBlocks(source string) (string, map[int]string, error) {
	blocks := map[int]string{}
	matches := asmBlockRegex.FindAllStringIndex(source, -1)
	if len(matches) == 0 {
		return source, blocks, nil
	}
	var out strings.Builder
	last := 0
	for _, match := range matches {
		braceIndex := strings.Index(source[match[0]:], "{") + match[0]
		closeIndex := MatchingBrace(source, braceIndex)
		if closeIndex == -1 {
			return "", nil, fmt.Errorf("Unterminated asm block")
		}
		lineEnd := strings.Index(source[closeIndex:], "\n")
		if lineEnd == -1 {
			lineEnd = len(source)
		} else {
			lineEnd += closeIndex
		}
		tail := strings.TrimSpace(StripComment(source[closeIndex+1 : lineEnd]))
		if tail != "" {
			return "", nil, fmt.Errorf("Unexpected text after asm block: %q", tail)
		}
		out.WriteString(source[last:match[0]])
		out.WriteString(fmt.Sprintf("\x00ASM%d\x00", len(blocks)))
		blocks[len(blocks)] = source[braceIndex+1 : closeIndex]
		last = lineEnd
	}
	out.WriteString(source[last:])
	return out.String(), blocks, nil
}

func (parser *PenaParser) parseInlineAssembly(placeholder string) error {
	idText := strings.TrimSuffix(strings.TrimPrefix(placeholder, "\x00ASM"), "\x00")
	blockID := 0
	if _, err := fmt.Sscanf(idText, "%d", &blockID); err != nil {
		return fmt.Errorf("invalid inline assembly placeholder: %q", placeholder)
	}
	namespace := fmt.Sprintf(".A%d.", blockID)
	instructions, err := ParseLines(strings.Split(parser.asmBlocks[blockID], "\n"), namespace)
	if err != nil {
		return err
	}
	parser.program = append(parser.program, instructions...)
	return nil
}

// ---------------------------------------------------------------------- #
// Source preparation
// ---------------------------------------------------------------------- #

func (parser *PenaParser) preprocess(source string) []string {
	lines := []string{}
	for _, raw := range strings.Split(source, "\n") {
		stripped := strings.TrimSpace(raw)
		if stripped == "" || strings.HasPrefix(stripped, "//") {
			continue
		}
		lines = append(lines, splitBraces(stripped)...)
	}
	return lines
}

func splitBraces(line string) []string {
	pieces := []string{}
	var current strings.Builder
	inString := false
	escape := false
	flush := func() {
		piece := strings.TrimSpace(current.String())
		if piece != "" {
			pieces = append(pieces, piece)
		}
		current.Reset()
	}
	for i := 0; i < len(line); i++ {
		char := line[i]
		switch {
		case escape:
			current.WriteByte(char)
			escape = false
		case char == '\\' && inString:
			current.WriteByte(char)
			escape = true
		case char == '"':
			inString = !inString
			current.WriteByte(char)
		case !inString && char == '{':
			lookahead := i + 1
			for lookahead < len(line) && (line[lookahead] == ' ' || line[lookahead] == '\t') {
				lookahead++
			}
			if lookahead < len(line) && line[lookahead] == '}' {
				current.WriteString("{}")
				i = lookahead
				continue
			}
			flush()
			pieces = append(pieces, "{")
		case !inString && char == '}':
			flush()
			pieces = append(pieces, "}")
		default:
			current.WriteByte(char)
		}
	}
	flush()
	return pieces
}

// ---------------------------------------------------------------------- #
// Blocks and statements
// ---------------------------------------------------------------------- #

func (parser *PenaParser) parseBlock(lines []string, index int) error {
	i := index
	for i < len(lines) {
		line := lines[i]
		switch {
		case line == "}":
			return nil
		case line == "{":
			i++
		case strings.HasPrefix(line, "\x00ASM") && strings.HasSuffix(line, "\x00"):
			if err := parser.parseInlineAssembly(line); err != nil {
				return err
			}
			i++
		case strings.HasPrefix(line, "function "):
			next, err := parser.parseFunction(lines, i)
			if err != nil {
				return err
			}
			i = next
		case strings.HasPrefix(line, "for "):
			next, err := parser.parseFor(lines, i)
			if err != nil {
				return err
			}
			i = next
		case strings.HasPrefix(line, "while "):
			next, err := parser.parseWhile(lines, i)
			if err != nil {
				return err
			}
			i = next
		case strings.HasPrefix(line, "if "):
			next, err := parser.parseIf(lines, i)
			if err != nil {
				return err
			}
			i = next
		case strings.HasPrefix(line, "else"):
			if err := parser.parseBlock(lines, i+1); err != nil {
				return err
			}
			// `else` consumes the following block; skip to its end.
			i = skipBlock(lines, i+1)
		case strings.HasPrefix(line, "woof "):
			if err := parser.parseFunctionCall(line); err != nil {
				return err
			}
			i++
		case strings.HasPrefix(line, "print("):
			if err := parser.parsePrint(line); err != nil {
				return err
			}
			i++
		case strings.HasPrefix(line, "return"):
			if err := parser.parseReturn(line); err != nil {
				return err
			}
			i++
		case line == "break":
			if err := parser.emitBreak(); err != nil {
				return err
			}
			i++
		case line == "continue":
			if err := parser.emitContinue(); err != nil {
				return err
			}
			i++
		case strings.Contains(line, ":="):
			if err := parser.parseStructLiteral(line); err != nil {
				return err
			}
			i++
		case strings.Contains(line, "="):
			if err := parser.parseAssignment(line); err != nil {
				return err
			}
			i++
		default:
			if err := rejectBareAssembly(line); err != nil {
				return err
			}
			i++
		}
	}
	return nil
}

func skipBlock(lines []string, index int) int {
	depth := 0
	for i := index; i < len(lines); i++ {
		switch lines[i] {
		case "{":
			depth++
		case "}":
			depth--
			if depth <= 0 {
				return i + 1
			}
		}
	}
	return len(lines)
}

func rejectBareAssembly(line string) error {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return nil
	}
	word := strings.ToUpper(parts[0])
	if _, ok := Mnemonics[word]; ok {
		return fmt.Errorf("Bare assembly instruction %q; wrap it in an 'asm { ... }' block or deploy with language='asm'", parts[0])
	}
	if _, ok := VarMacros[word]; ok || word == "FUNC" {
		return fmt.Errorf("Bare assembly instruction %q; wrap it in an 'asm { ... }' block or deploy with language='asm'", parts[0])
	}
	return nil
}

func (parser *PenaParser) parseFunction(lines []string, index int) (int, error) {
	match := functionRegex.FindStringSubmatch(lines[index])
	if match == nil {
		return 0, fmt.Errorf("Invalid function header: %q", lines[index])
	}
	name := match[1]
	params := []string{}
	for _, param := range strings.Split(match[2], ",") {
		param = strings.TrimSpace(param)
		if param != "" {
			params = append(params, param)
		}
	}
	bodyLabel := parser.newLabel()
	skipLabel := parser.newLabel()

	parser.emit("PUSH", name)
	for _, param := range params {
		parser.emit("PUSH", param)
	}
	parser.emit("PUSH", int64(len(params)))
	parser.emit("DEF_FUNC", bodyLabel)
	parser.emit("JMP", skipLabel)

	parser.label(bodyLabel)
	if err := parser.parseBlock(lines, index+1); err != nil {
		return 0, err
	}
	parser.emit("END_FUNC")
	parser.label(skipLabel)
	return skipBlock(lines, index+1), nil
}

func (parser *PenaParser) parseFor(lines []string, index int) (int, error) {
	match := forRegex.FindStringSubmatch(lines[index])
	if match == nil {
		return 0, fmt.Errorf("Invalid for header: %q", lines[index])
	}
	variable := match[1]
	start, _ := parseIntLiteral(match[2])
	end, _ := parseIntLiteral(match[3])

	checkLabel := parser.newLabel()
	continueLabel := parser.newLabel()
	endLabel := parser.newLabel()

	parser.emit("PUSH", variable)
	parser.emit("PUSH", start)
	parser.emit("SET")

	parser.label(checkLabel)
	parser.emit("PUSH", variable)
	parser.emit("GET")
	parser.emit("PUSH", end)
	parser.emit("LT")
	parser.emit("JZ", endLabel)

	parser.loopStack = append(parser.loopStack, [2]string{continueLabel, endLabel})
	if err := parser.parseBlock(lines, index+1); err != nil {
		return 0, err
	}
	parser.loopStack = parser.loopStack[:len(parser.loopStack)-1]

	parser.label(continueLabel)
	parser.emit("PUSH", variable)
	parser.emit("PUSH", variable)
	parser.emit("GET")
	parser.emit("PUSH", int64(1))
	parser.emit("ADD")
	parser.emit("SET")
	parser.emit("JMP", checkLabel)
	parser.label(endLabel)
	return skipBlock(lines, index+1), nil
}

func (parser *PenaParser) parseWhile(lines []string, index int) (int, error) {
	continueLabel := parser.newLabel()
	endLabel := parser.newLabel()

	parser.label(continueLabel)
	condition := extractCondition(lines[index])
	if err := parser.compileExpression(tokenizeExpression(condition)); err != nil {
		return 0, err
	}
	parser.emit("JZ", endLabel)

	parser.loopStack = append(parser.loopStack, [2]string{continueLabel, endLabel})
	if err := parser.parseBlock(lines, index+1); err != nil {
		return 0, err
	}
	parser.loopStack = parser.loopStack[:len(parser.loopStack)-1]

	parser.emit("JMP", continueLabel)
	parser.label(endLabel)
	return skipBlock(lines, index+1), nil
}

func (parser *PenaParser) parseIf(lines []string, index int) (int, error) {
	endLabel := parser.newLabel()
	i, err := parser.parseConditionalBranch(lines, index, endLabel)
	if err != nil {
		return 0, err
	}
	for i < len(lines) && strings.HasPrefix(lines[i], "else if") {
		i, err = parser.parseConditionalBranch(lines, i, endLabel)
		if err != nil {
			return 0, err
		}
	}
	if i < len(lines) && strings.HasPrefix(lines[i], "else") {
		if err := parser.parseBlock(lines, i+1); err != nil {
			return 0, err
		}
		i = skipBlock(lines, i+1)
	}
	parser.label(endLabel)
	return i, nil
}

func (parser *PenaParser) parseConditionalBranch(lines []string, index int, endLabel string) (int, error) {
	nextLabel := parser.newLabel()
	condition := extractCondition(lines[index])
	if err := parser.compileExpression(tokenizeExpression(condition)); err != nil {
		return 0, err
	}
	parser.emit("JZ", nextLabel)
	if err := parser.parseBlock(lines, index+1); err != nil {
		return 0, err
	}
	parser.emit("JMP", endLabel)
	parser.label(nextLabel)
	return skipBlock(lines, index+1), nil
}

func extractCondition(line string) string {
	match := conditionRegex.FindStringSubmatch(line)
	if match == nil {
		return ""
	}
	return match[1]
}

// ---------------------------------------------------------------------- #
// Simple statements
// ---------------------------------------------------------------------- #

func (parser *PenaParser) parseAssignment(line string) error {
	target := ""
	expression := ""
	if index := strings.Index(line, "="); index >= 0 {
		target = strings.TrimSpace(line[:index])
		expression = strings.TrimSpace(line[index+1:])
	}
	if match := subscriptRegex.FindStringSubmatch(target); match != nil {
		parser.emit("PUSH", match[1])
		if err := parser.compileExpression(tokenizeExpression(match[2])); err != nil {
			return err
		}
		if err := parser.compileExpression(tokenizeExpression(expression)); err != nil {
			return err
		}
		parser.emit("DICT_SET")
		return nil
	}
	parser.emit("PUSH", target)
	if err := parser.compileExpression(tokenizeExpression(expression)); err != nil {
		return err
	}
	parser.emit("SET")
	return nil
}

func (parser *PenaParser) parseStructLiteral(line string) error {
	index := strings.Index(line, ":=")
	variable := strings.TrimSpace(line[:index])
	value := strings.TrimSpace(line[index+2:])

	if strings.HasPrefix(value, "[") {
		inner := value[1:]
		if strings.HasSuffix(value, "]") {
			inner = value[1 : len(value)-1]
		}
		items := splitArguments(inner)
		parser.emit("PUSH", variable)
		parser.emit("PUSH", []any{})
		parser.emit("SET")
		for _, item := range items {
			parser.emit("PUSH", variable)
			if err := parser.compileExpression(tokenizeExpression(item)); err != nil {
				return err
			}
			parser.emit("LIST_APPEND")
		}
		return nil
	}
	if strings.HasPrefix(value, "{") {
		parser.emit("PUSH", variable)
		parser.emit("PUSH", map[string]any{})
		parser.emit("SET")
		return nil
	}
	return parser.parseAssignment(strings.Replace(line, ":=", "=", 1))
}

func (parser *PenaParser) parsePrint(line string) error {
	match := regexp.MustCompile(`^print\((.*)\)$`).FindStringSubmatch(line)
	expression := ""
	if match != nil {
		expression = match[1]
	}
	if strings.TrimSpace(expression) != "" {
		if err := parser.compileExpression(tokenizeExpression(expression)); err != nil {
			return err
		}
	} else {
		parser.emit("PUSH", "")
	}
	parser.emit("PRINT")
	return nil
}

func (parser *PenaParser) parseReturn(line string) error {
	expression := strings.TrimSpace(line[len("return"):])
	if expression != "" {
		if err := parser.compileExpression(tokenizeExpression(expression)); err != nil {
			return err
		}
	}
	parser.emit("RET")
	return nil
}

func (parser *PenaParser) parseFunctionCall(line string) error {
	inner := strings.TrimSpace(line[len("woof "):])
	match := callRegex.FindStringSubmatch(inner)
	if match == nil {
		return fmt.Errorf("Invalid function call: %q", line)
	}
	arguments := splitArguments(match[2])
	for _, argument := range arguments {
		if err := parser.compileExpression(tokenizeExpression(argument)); err != nil {
			return err
		}
	}
	parser.emit("PUSH", match[1])
	parser.emit("PUSH", int64(len(arguments)))
	parser.emit("CALL_FUNC")
	return nil
}

// ---------------------------------------------------------------------- #
// Loop control
// ---------------------------------------------------------------------- #

func (parser *PenaParser) emitBreak() error {
	if len(parser.loopStack) == 0 {
		return fmt.Errorf("'break' used outside of a loop")
	}
	parser.emit("JMP", parser.loopStack[len(parser.loopStack)-1][1])
	return nil
}

func (parser *PenaParser) emitContinue() error {
	if len(parser.loopStack) == 0 {
		return fmt.Errorf("'continue' used outside of a loop")
	}
	parser.emit("JMP", parser.loopStack[len(parser.loopStack)-1][0])
	return nil
}

// ---------------------------------------------------------------------- #
// Expressions (recursive descent)
// ---------------------------------------------------------------------- #

func (parser *PenaParser) compileExpression(tokens []string) error {
	parser.tokens = tokens
	parser.position = 0
	if len(tokens) == 0 {
		return nil
	}
	if err := parser.parseOr(); err != nil {
		return err
	}
	if parser.position < len(parser.tokens) {
		return fmt.Errorf("Unexpected token in expression: %q", parser.tokens[parser.position])
	}
	return nil
}

func (parser *PenaParser) peek() string {
	if parser.position < len(parser.tokens) {
		return parser.tokens[parser.position]
	}
	return ""
}

func (parser *PenaParser) hasPeek() bool {
	return parser.position < len(parser.tokens)
}

func (parser *PenaParser) next() string {
	token := parser.peek()
	parser.position++
	return token
}

func (parser *PenaParser) parseOr() error {
	if err := parser.parseAnd(); err != nil {
		return err
	}
	for parser.hasPeek() && parser.peek() == "||" {
		parser.next()
		if err := parser.parseAnd(); err != nil {
			return err
		}
		parser.emit("OR")
	}
	return nil
}

func (parser *PenaParser) parseAnd() error {
	if err := parser.parseComparison(); err != nil {
		return err
	}
	for parser.hasPeek() && parser.peek() == "&&" {
		parser.next()
		if err := parser.parseComparison(); err != nil {
			return err
		}
		parser.emit("AND")
	}
	return nil
}

func (parser *PenaParser) parseComparison() error {
	if err := parser.parseAdditive(); err != nil {
		return err
	}
	for parser.hasPeek() && comparisonOperators[parser.peek()] {
		operator := parser.next()
		if err := parser.parseAdditive(); err != nil {
			return err
		}
		parser.emit(binaryMnemonics[operator])
	}
	return nil
}

func (parser *PenaParser) parseAdditive() error {
	if err := parser.parseMultiplicative(); err != nil {
		return err
	}
	for parser.hasPeek() && (parser.peek() == "+" || parser.peek() == "-") {
		operator := parser.next()
		if err := parser.parseMultiplicative(); err != nil {
			return err
		}
		parser.emit(binaryMnemonics[operator])
	}
	return nil
}

func (parser *PenaParser) parseMultiplicative() error {
	if err := parser.parseUnary(); err != nil {
		return err
	}
	for parser.hasPeek() && (parser.peek() == "*" || parser.peek() == "/" || parser.peek() == "%") {
		operator := parser.next()
		if err := parser.parseUnary(); err != nil {
			return err
		}
		parser.emit(binaryMnemonics[operator])
	}
	return nil
}

func (parser *PenaParser) parseUnary() error {
	if parser.hasPeek() && parser.peek() == "-" {
		parser.next()
		parser.emit("PUSH", int64(0))
		if err := parser.parseUnary(); err != nil {
			return err
		}
		parser.emit("SUB")
		return nil
	}
	if parser.hasPeek() && parser.peek() == "!" {
		parser.next()
		if err := parser.parseUnary(); err != nil {
			return err
		}
		parser.emit("PUSH", int64(0))
		parser.emit("EQ")
		return nil
	}
	return parser.parsePrimary()
}

func (parser *PenaParser) parsePrimary() error {
	if !parser.hasPeek() {
		return fmt.Errorf("Unexpected end of expression")
	}
	token := parser.next()

	if strings.HasPrefix(token, "\"") {
		value, err := parseQuotedString(token)
		if err != nil {
			return err
		}
		parser.emit("PUSH", value)
		return nil
	}

	if numberRegex2.MatchString(token) {
		if strings.Contains(token, ".") {
			floatValue, err := parseFloatToken(token)
			if err != nil {
				return err
			}
			parser.emit("PUSH", floatValue)
		} else {
			integerValue, err := parseIntegerToken(token)
			if err != nil {
				return err
			}
			parser.emit("PUSH", integerValue)
		}
		return nil
	}

	if isIdentifier(token) {
		if parser.hasPeek() && parser.peek() == "[" {
			parser.next()
			parser.emit("PUSH", token)
			if err := parser.parseOr(); err != nil {
				return err
			}
			if !parser.hasPeek() || parser.next() != "]" {
				return fmt.Errorf("Expected ']' in subscript expression")
			}
			parser.emit("DICT_GET")
			return nil
		}
		if parser.hasPeek() && parser.peek() == "(" {
			return parser.parseCallArguments(token)
		}
		parser.emit("PUSH", token)
		parser.emit("GET")
		return nil
	}

	if token == "(" {
		if err := parser.parseOr(); err != nil {
			return err
		}
		if !parser.hasPeek() || parser.next() != ")" {
			return fmt.Errorf("Expected ')' in expression")
		}
		return nil
	}

	return fmt.Errorf("Unexpected token in expression: %q", token)
}

func parseQuotedString(token string) (string, error) {
	parser := &literalParser{text: token}
	value, err := parser.parseValue()
	if err != nil {
		return "", err
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("invalid string literal")
	}
	return text, nil
}

func parseFloatToken(token string) (float64, error) {
	parser := &literalParser{text: token}
	value, err := parser.parseValue()
	if err != nil {
		return 0, err
	}
	floatValue, ok := value.(float64)
	if !ok {
		return 0, fmt.Errorf("invalid float literal")
	}
	return floatValue, nil
}

func parseIntegerToken(token string) (any, error) {
	parser := &literalParser{text: token}
	value, err := parser.parseValue()
	if err != nil {
		return 0, err
	}
	if _, ok := value.(*big.Int); ok {
		return value, nil
	}
	if _, ok := value.(int64); ok {
		return value, nil
	}
	return nil, fmt.Errorf("invalid integer literal")
}

func isIdentifier(token string) bool {
	if token == "" {
		return false
	}
	for i := 0; i < len(token); i++ {
		char := token[i]
		if i == 0 {
			if !(char == '_' || char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z') {
				return false
			}
		} else if !(char == '_' || char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' ||
			char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}

func (parser *PenaParser) parseCallArguments(name string) error {
	parser.next() // consume '('
	argumentCount := 0
	if parser.hasPeek() && parser.peek() != ")" {
		for {
			if err := parser.parseOr(); err != nil {
				return err
			}
			argumentCount++
			if parser.hasPeek() && parser.peek() == "," {
				parser.next()
				continue
			}
			break
		}
	}
	if !parser.hasPeek() || parser.next() != ")" {
		return fmt.Errorf("Expected ')' in call expression")
	}
	parser.emit("PUSH", name)
	parser.emit("PUSH", int64(argumentCount))
	parser.emit("CALL_FUNC")
	return nil
}

func tokenizeExpression(expression string) []string {
	return tokenRegex.FindAllString(expression, -1)
}

func splitArguments(raw string) []string {
	arguments := []string{}
	var current strings.Builder
	inString := false
	escape := false
	for i := 0; i < len(raw); i++ {
		char := raw[i]
		switch {
		case escape:
			current.WriteByte(char)
			escape = false
		case char == '\\' && inString:
			current.WriteByte(char)
			escape = true
		case char == '"':
			inString = !inString
			current.WriteByte(char)
		case char == ',' && !inString:
			argument := strings.TrimSpace(current.String())
			if argument != "" {
				arguments = append(arguments, argument)
			}
			current.Reset()
		default:
			current.WriteByte(char)
		}
	}
	tail := strings.TrimSpace(current.String())
	if tail != "" {
		arguments = append(arguments, tail)
	}
	return arguments
}
