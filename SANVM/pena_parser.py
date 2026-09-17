import ast
import re
from typing import Any, List

from SANVM.OpCode import OpCode

TOKEN_RE = re.compile(
    r'"(?:[^"\\]|\\.)*"|\d+\.\d+|\d+|[A-Za-z_]\w*|==|!=|<=|>=|&&|\|\||[-+*/%<>()\[\],!]'
)

LABEL_PREFIX = "\x00L"

BINARY_OPCODES = {
    "+": OpCode.ADD,
    "-": OpCode.SUB,
    "*": OpCode.MUL,
    "/": OpCode.DIV,
    "%": OpCode.MOD,
    "==": OpCode.EQ,
    "!=": OpCode.NEQ,
    "<": OpCode.LT,
    "<=": OpCode.LTE,
    ">": OpCode.GT,
    ">=": OpCode.GTE,
}

COMPARISON_OPERATORS = ("==", "!=", "<", "<=", ">", ">=")

TARGET_OPCODES = {
    OpCode.JMP.value,
    OpCode.JZ.value,
    OpCode.JNZ.value,
    OpCode.DEF_FUNC.value,
}


class _LabelMarker:
    """Marks a position in the bytecode stream; removed during resolution."""

    __slots__ = ("name",)

    def __init__(self, name: str):
        self.name = name


class PenaParser:
    """Compiles PENA source into SANVM bytecode.

    The compiler is two-pass: statements emit symbolic labels which are
    resolved to real instruction indices before execution, so jumps always
    point at valid positions.
    """

    def __init__(self):
        self.bytecode: List[Any] = []
        self.label_counter = 0
        self.loop_stack: list[tuple[str, str]] = []
        self._tokens: List[str] = []
        self._position = 0

    # ------------------------------------------------------------------ #
    # Entry point
    # ------------------------------------------------------------------ #

    def parse(self, source: str) -> List[Any]:
        self.bytecode = []
        self.label_counter = 0
        self.loop_stack = []
        lines = self._preprocess(source)
        self._parse_block(lines, 0)
        return self._resolve_labels(self.bytecode)

    # ------------------------------------------------------------------ #
    # Source preparation
    # ------------------------------------------------------------------ #

    def _preprocess(self, source: str) -> List[str]:
        lines: List[str] = []
        for raw in source.splitlines():
            stripped = raw.strip()
            if not stripped or stripped.startswith("//"):
                continue
            lines.extend(self._split_braces(stripped))
        return lines

    @staticmethod
    def _split_braces(line: str) -> List[str]:
        """Split block braces onto their own lines (string literals respected).

        ``{}`` is kept as a single piece so empty dict literals survive.
        """
        pieces: List[str] = []
        current: List[str] = []
        in_string = False
        escape = False
        i = 0

        def flush():
            piece = "".join(current).strip()
            if piece:
                pieces.append(piece)
            current.clear()

        while i < len(line):
            char = line[i]

            if escape:
                current.append(char)
                escape = False
                i += 1
                continue
            if char == "\\" and in_string:
                current.append(char)
                escape = True
                i += 1
                continue
            if char == '"':
                in_string = not in_string
                current.append(char)
                i += 1
                continue

            if not in_string and char == "{":
                lookahead = i + 1
                while lookahead < len(line) and line[lookahead] in " \t":
                    lookahead += 1
                if lookahead < len(line) and line[lookahead] == "}":
                    current.append("{}")
                    i = lookahead + 1
                    continue

                flush()
                pieces.append("{")
                i += 1
                continue

            if not in_string and char == "}":
                flush()
                pieces.append("}")
                i += 1
                continue

            current.append(char)
            i += 1

        flush()
        return pieces

    # ------------------------------------------------------------------ #
    # Blocks and statements
    # ------------------------------------------------------------------ #

    def _parse_block(self, lines: List[str], i: int) -> int:
        while i < len(lines):
            line = lines[i]

            if line == "}":
                return i + 1
            if line == "{":
                i += 1
                continue

            if line.startswith("function "):
                i = self._parse_function(lines, i)
            elif line.startswith("for "):
                i = self._parse_for(lines, i)
            elif line.startswith("while "):
                i = self._parse_while(lines, i)
            elif line.startswith("if "):
                i = self._parse_if(lines, i)
            elif line.startswith("else"):
                i = self._parse_block(lines, i + 1)
            elif line.startswith("woof "):
                self._parse_function_call(line)
                i += 1
            elif line.startswith("print("):
                self._parse_print(line)
                i += 1
            elif line.startswith("return"):
                self._parse_return(line)
                i += 1
            elif line == "break":
                self._emit_break()
                i += 1
            elif line == "continue":
                self._emit_continue()
                i += 1
            elif ":=" in line:
                self._parse_struct_literal(line)
                i += 1
            elif "=" in line:
                self._parse_assignment(line)
                i += 1
            else:
                i += 1

        return i

    def _parse_function(self, lines: List[str], i: int) -> int:
        match = re.match(r"function\s+(\w+)\s*\((.*?)\)", lines[i])
        if not match:
            raise ValueError(f"Invalid function header: {lines[i]!r}")

        name = match.group(1)
        params = [param.strip() for param in match.group(2).split(",") if param.strip()]

        body_label = self._new_label()
        skip_label = self._new_label()

        self.bytecode.extend([OpCode.PUSH.value, name])
        for param in params:
            self.bytecode.extend([OpCode.PUSH.value, param])
        self.bytecode.extend(
            [
                OpCode.PUSH.value,
                len(params),
                OpCode.DEF_FUNC.value,
                body_label,
                OpCode.JMP.value,
                skip_label,
            ]
        )

        self._mark(body_label)
        i = self._parse_block(lines, i + 1)
        self.bytecode.append(OpCode.END_FUNC.value)
        self._mark(skip_label)
        return i

    def _parse_for(self, lines: List[str], i: int) -> int:
        match = re.match(r"for\s+(\w+)\s*,\s*(-?\d+)\s*->\s*(-?\d+)", lines[i])
        if not match:
            raise ValueError(f"Invalid for header: {lines[i]!r}")

        var, start, end = match.group(1), int(match.group(2)), int(match.group(3))
        check_label = self._new_label()
        continue_label = self._new_label()
        end_label = self._new_label()

        # var = start
        self.bytecode.extend(
            [OpCode.PUSH.value, var, OpCode.PUSH.value, start, OpCode.SET.value]
        )

        self._mark(check_label)
        self.bytecode.extend(
            [
                OpCode.PUSH.value,
                var,
                OpCode.GET.value,
                OpCode.PUSH.value,
                end,
                OpCode.LT.value,
                OpCode.JZ.value,
                end_label,
            ]
        )

        # `continue` jumps here: increment first, then re-check the condition.
        self.loop_stack.append((continue_label, end_label))
        i = self._parse_block(lines, i + 1)
        self.loop_stack.pop()

        self._mark(continue_label)
        self.bytecode.extend(
            [
                OpCode.PUSH.value,
                var,
                OpCode.PUSH.value,
                var,
                OpCode.GET.value,
                OpCode.PUSH.value,
                1,
                OpCode.ADD.value,
                OpCode.SET.value,
            ]
        )
        self.bytecode.extend([OpCode.JMP.value, check_label])
        self._mark(end_label)
        return i

    def _parse_while(self, lines: List[str], i: int) -> int:
        continue_label = self._new_label()
        end_label = self._new_label()

        self._mark(continue_label)
        self._compile_expression(self._tokenize_expression(self._extract_condition(lines[i])))
        self.bytecode.extend([OpCode.JZ.value, end_label])

        self.loop_stack.append((continue_label, end_label))
        i = self._parse_block(lines, i + 1)
        self.loop_stack.pop()

        self.bytecode.extend([OpCode.JMP.value, continue_label])
        self._mark(end_label)
        return i

    def _parse_if(self, lines: List[str], i: int) -> int:
        end_label = self._new_label()

        i = self._parse_conditional_branch(lines, i, end_label)
        while i < len(lines) and lines[i].startswith("else if"):
            i = self._parse_conditional_branch(lines, i, end_label)

        if i < len(lines) and lines[i].startswith("else"):
            i = self._parse_block(lines, i + 1)

        self._mark(end_label)
        return i

    def _parse_conditional_branch(self, lines: List[str], i: int, end_label: str) -> int:
        next_label = self._new_label()

        self._compile_expression(self._tokenize_expression(self._extract_condition(lines[i])))
        self.bytecode.extend([OpCode.JZ.value, next_label])

        i = self._parse_block(lines, i + 1)

        self.bytecode.extend([OpCode.JMP.value, end_label])
        self._mark(next_label)
        return i

    # ------------------------------------------------------------------ #
    # Simple statements
    # ------------------------------------------------------------------ #

    def _parse_assignment(self, line: str):
        target, expr = map(str.strip, line.split("=", 1))

        subscript = re.match(r"^(\w+)\s*\[(.*)\]$", target)
        if subscript:
            name, index_expr = subscript.group(1), subscript.group(2)
            self.bytecode.extend([OpCode.PUSH.value, name])
            self._compile_expression(self._tokenize_expression(index_expr))
            self._compile_expression(self._tokenize_expression(expr))
            self.bytecode.append(OpCode.DICT_SET.value)
            return

        self.bytecode.extend([OpCode.PUSH.value, target])
        self._compile_expression(self._tokenize_expression(expr))
        self.bytecode.append(OpCode.SET.value)

    def _parse_struct_literal(self, line: str):
        # Example: mylist := [1, 2, 3]
        var, value = map(str.strip, line.split(":=", 1))

        if value.startswith("["):
            inner = value[1:value.rindex("]")] if value.endswith("]") else value[1:]
            items = self._split_arguments(inner)
            self.bytecode.extend(
                [OpCode.PUSH.value, var, OpCode.PUSH.value, [], OpCode.SET.value]
            )
            for item in items:
                self.bytecode.extend([OpCode.PUSH.value, var])
                self._compile_expression(self._tokenize_expression(item))
                self.bytecode.append(OpCode.LIST_APPEND.value)
            return

        if value.startswith("{"):
            self.bytecode.extend(
                [OpCode.PUSH.value, var, OpCode.PUSH.value, {}, OpCode.SET.value]
            )
            return

        self._parse_assignment(line.replace(":=", "=", 1))

    def _parse_print(self, line: str):
        match = re.match(r"print\((.*)\)", line)
        expr = match.group(1) if match else ""
        if expr.strip():
            self._compile_expression(self._tokenize_expression(expr))
        else:
            self.bytecode.extend([OpCode.PUSH.value, ""])
        self.bytecode.append(OpCode.PRINT.value)

    def _parse_return(self, line: str):
        expr = line[len("return"):].strip()
        if expr:
            self._compile_expression(self._tokenize_expression(expr))
        self.bytecode.append(OpCode.RET.value)

    def _parse_function_call(self, line: str):
        inner = line[len("woof "):].strip()
        match = re.match(r"(\w+)\s*\((.*)\)", inner)
        if not match:
            raise ValueError(f"Invalid function call: {line!r}")

        name, args_raw = match.group(1), match.group(2)
        args = self._split_arguments(args_raw)

        for arg in args:
            self._compile_expression(self._tokenize_expression(arg))

        self.bytecode.extend(
            [
                OpCode.PUSH.value,
                name,
                OpCode.PUSH.value,
                len(args),
                OpCode.CALL_FUNC.value,
            ]
        )

    # ------------------------------------------------------------------ #
    # Loop control
    # ------------------------------------------------------------------ #

    def _emit_break(self):
        if not self.loop_stack:
            raise ValueError("'break' used outside of a loop")
        _, end_label = self.loop_stack[-1]
        self.bytecode.extend([OpCode.JMP.value, end_label])

    def _emit_continue(self):
        if not self.loop_stack:
            raise ValueError("'continue' used outside of a loop")
        continue_label, _ = self.loop_stack[-1]
        self.bytecode.extend([OpCode.JMP.value, continue_label])

    # ------------------------------------------------------------------ #
    # Expressions (recursive descent)
    # ------------------------------------------------------------------ #

    def _compile_expression(self, tokens: List[str]):
        self._tokens = tokens
        self._position = 0
        if not tokens:
            return
        self._parse_or()
        if self._position < len(self._tokens):
            raise ValueError(f"Unexpected token in expression: {self._tokens[self._position]!r}")

    def _peek(self):
        if self._position < len(self._tokens):
            return self._tokens[self._position]
        return None

    def _next(self):
        token = self._peek()
        self._position += 1
        return token

    def _parse_or(self):
        self._parse_and()
        while self._peek() == "||":
            self._next()
            self._parse_and()
            self.bytecode.append(OpCode.OR.value)

    def _parse_and(self):
        self._parse_comparison()
        while self._peek() == "&&":
            self._next()
            self._parse_comparison()
            self.bytecode.append(OpCode.AND.value)

    def _parse_comparison(self):
        self._parse_additive()
        while self._peek() in COMPARISON_OPERATORS:
            operator = self._next()
            self._parse_additive()
            self.bytecode.append(BINARY_OPCODES[operator].value)

    def _parse_additive(self):
        self._parse_multiplicative()
        while self._peek() in ("+", "-"):
            operator = self._next()
            self._parse_multiplicative()
            self.bytecode.append(BINARY_OPCODES[operator].value)

    def _parse_multiplicative(self):
        self._parse_unary()
        while self._peek() in ("*", "/", "%"):
            operator = self._next()
            self._parse_unary()
            self.bytecode.append(BINARY_OPCODES[operator].value)

    def _parse_unary(self):
        if self._peek() == "-":
            self._next()
            self.bytecode.extend([OpCode.PUSH.value, 0])
            self._parse_unary()
            self.bytecode.append(OpCode.SUB.value)
            return
        if self._peek() == "!":
            self._next()
            self._parse_unary()
            self.bytecode.extend([OpCode.PUSH.value, 0, OpCode.EQ.value])
            return
        self._parse_primary()

    def _parse_primary(self):
        token = self._next()
        if token is None:
            raise ValueError("Unexpected end of expression")

        if token.startswith('"'):
            self.bytecode.extend([OpCode.PUSH.value, ast.literal_eval(token)])
            return

        if self._is_number(token):
            value = int(token) if "." not in token else float(token)
            self.bytecode.extend([OpCode.PUSH.value, value])
            return

        if token.isidentifier():
            if self._peek() == "[":
                self._next()  # consume '['
                self.bytecode.extend([OpCode.PUSH.value, token])
                self._parse_or()
                closing = self._next()
                if closing != "]":
                    raise ValueError(f"Expected ']' but found {closing!r}")
                self.bytecode.append(OpCode.DICT_GET.value)
                return

            if self._peek() == "(":
                self._parse_call_arguments(token)
                return

            self.bytecode.extend([OpCode.PUSH.value, token, OpCode.GET.value])
            return

        if token == "(":
            self._parse_or()
            closing = self._next()
            if closing != ")":
                raise ValueError(f"Expected ')' but found {closing!r}")
            return

        raise ValueError(f"Unexpected token in expression: {token!r}")

    @staticmethod
    def _is_number(token: str) -> bool:
        return bool(re.fullmatch(r"\d+(\.\d+)?", token))

    def _parse_call_arguments(self, name: str):
        """Compile ``name(arg1, arg2, ...)`` used inside an expression."""
        self._next()  # consume '('

        arg_count = 0
        if self._peek() != ")":
            while True:
                self._parse_or()
                arg_count += 1
                if self._peek() == ",":
                    self._next()
                    continue
                break

        closing = self._next()
        if closing != ")":
            raise ValueError(f"Expected ')' but found {closing!r}")

        self.bytecode.extend(
            [
                OpCode.PUSH.value,
                name,
                OpCode.PUSH.value,
                arg_count,
                OpCode.CALL_FUNC.value,
            ]
        )

    def _tokenize_expression(self, expr: str) -> List[str]:
        return TOKEN_RE.findall(expr)

    # ------------------------------------------------------------------ #
    # Labels and argument splitting
    # ------------------------------------------------------------------ #

    def _new_label(self) -> str:
        self.label_counter += 1
        return f"{LABEL_PREFIX}{self.label_counter}"

    def _mark(self, label: str):
        self.bytecode.append(_LabelMarker(label))

    def _resolve_labels(self, bytecode: List[Any]) -> List[Any]:
        positions: dict[str, int] = {}
        flat: List[Any] = []
        for item in bytecode:
            if isinstance(item, _LabelMarker):
                positions[item.name] = len(flat)
            else:
                flat.append(item)

        resolved = []
        i = 0
        while i < len(flat):
            item = flat[i]
            resolved.append(item)

            next_item = flat[i + 1] if i + 1 < len(flat) else None
            if (
                isinstance(item, int)
                and item in TARGET_OPCODES
                and isinstance(next_item, str)
                and next_item.startswith(LABEL_PREFIX)
            ):
                if next_item not in positions:
                    raise ValueError(f"Unknown label: {next_item!r}")
                resolved.append(positions[next_item])
                i += 2
                continue

            i += 1

        return resolved

    @staticmethod
    def _extract_condition(line: str) -> str:
        match = re.search(r"\((.*)\)", line)
        if not match:
            raise ValueError(f"Missing condition in: {line!r}")
        return match.group(1)

    @staticmethod
    def _split_arguments(raw: str) -> List[str]:
        args: List[str] = []
        current: List[str] = []
        in_string = False
        escape = False

        for char in raw:
            if escape:
                current.append(char)
                escape = False
                continue
            if char == "\\" and in_string:
                current.append(char)
                escape = True
                continue
            if char == '"':
                in_string = not in_string
                current.append(char)
                continue
            if char == "," and not in_string:
                argument = "".join(current).strip()
                if argument:
                    args.append(argument)
                current = []
                continue
            current.append(char)

        tail = "".join(current).strip()
        if tail:
            args.append(tail)
        return args
