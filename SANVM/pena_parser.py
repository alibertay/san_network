"""High-level PENA compiler: lowers human-friendly syntax to PENA Assembly.

The parser no longer emits bytecode directly. Every construct is lowered to
assembly instructions (``(mnemonic, operands)`` records, see ``SANVM.asm``)
and the shared assembler resolves labels and encodes the final bytecode. This
keeps a single source of truth for instruction encoding and makes inline
``asm { ... }`` blocks first-class citizens of the high-level language.
"""

import ast
import re
from typing import Any, Dict, List, Tuple

from SANVM import asm
from SANVM.asm import LABEL, Instruction, assemble, encode, looks_like_assembly, parse_lines

TOKEN_RE = re.compile(
    r'"(?:[^"\\]|\\.)*"|\d+\.\d+|\d+|[A-Za-z_]\w*|==|!=|<=|>=|&&|\|\||[-+*/%<>()\[\],!]'
)

BINARY_MNEMONICS = {
    "+": "ADD",
    "-": "SUB",
    "*": "MUL",
    "/": "DIV",
    "%": "MOD",
    "==": "EQ",
    "!=": "NEQ",
    "<": "LT",
    "<=": "LTE",
    ">": "GT",
    ">=": "GTE",
}

COMPARISON_OPERATORS = ("==", "!=", "<", "<=", ">", ">=")

ASM_PLACEHOLDER_PREFIX = "\x00ASM"
ASM_PLACEHOLDER_SUFFIX = "\x00"


class PenaParser:
    """Compiles PENA source into SANVM bytecode through PENA Assembly.

    The compiler is two-pass: constructs emit assembly records with symbolic
    labels, then the assembler resolves labels to real instruction indices, so
    jumps always point at valid positions. ``asm { ... }`` blocks are parsed by
    the same assembler and merged into the surrounding program; their labels
    are namespaced per block so identical names cannot collide.
    """

    def __init__(self):
        self.program: List[Instruction] = []
        self.label_counter = 0
        self.loop_stack: list[tuple[str, str]] = []
        self._tokens: List[str] = []
        self._position = 0
        self._asm_blocks: Dict[int, str] = {}

    # ------------------------------------------------------------------ #
    # Entry point
    # ------------------------------------------------------------------ #

    def parse(self, source: str) -> List[Any]:
        source, self._asm_blocks = self._extract_asm_blocks(source)
        self.program = []
        self.label_counter = 0
        self.loop_stack = []
        lines = self._preprocess(source)
        self._parse_block(lines, 0)
        return encode(self.program)

    # ------------------------------------------------------------------ #
    # Emission helpers
    # ------------------------------------------------------------------ #

    def _emit(self, mnemonic: str, *operands: Any) -> None:
        self.program.append((mnemonic, list(operands)))

    def _label(self, name: str) -> None:
        self.program.append((LABEL, [name]))

    def _new_label(self) -> str:
        self.label_counter += 1
        return f".L{self.label_counter}"

    # ------------------------------------------------------------------ #
    # Inline assembly blocks
    # ------------------------------------------------------------------ #

    def _extract_asm_blocks(self, source: str) -> Tuple[str, Dict[int, str]]:
        """Replace ``asm { ... }`` blocks with placeholders (brace aware)."""
        pattern = re.compile(r"(?m)^[ \t]*asm[ \t]*\{")
        blocks: Dict[int, str] = {}
        out: List[str] = []
        last = 0

        for match in pattern.finditer(source):
            brace_index = source.index("{", match.start())
            close = asm.matching_brace(source, brace_index)
            if close == -1:
                raise ValueError("Unterminated asm block")
            line_end = source.find("\n", close)
            if line_end == -1:
                line_end = len(source)
            tail = asm.strip_comment(source[close + 1:line_end]).strip()
            if tail:
                raise ValueError(f"Unexpected text after asm block: {tail!r}")

            out.append(source[last:match.start()])
            out.append(f"{ASM_PLACEHOLDER_PREFIX}{len(blocks)}{ASM_PLACEHOLDER_SUFFIX}")
            blocks[len(blocks)] = source[brace_index + 1:close]
            last = line_end

        out.append(source[last:])
        return "".join(out), blocks

    def _parse_inline_assembly(self, placeholder: str) -> None:
        block_id = int(placeholder[len(ASM_PLACEHOLDER_PREFIX):-len(ASM_PLACEHOLDER_SUFFIX)])
        content = self._asm_blocks[block_id]
        self.program.extend(parse_lines(content.splitlines(), namespace=f".A{block_id}."))

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
            if line.startswith(ASM_PLACEHOLDER_PREFIX) and line.endswith(ASM_PLACEHOLDER_SUFFIX):
                self._parse_inline_assembly(line)
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
                self._reject_bare_assembly(line)
                i += 1

        return i

    @staticmethod
    def _reject_bare_assembly(line: str) -> None:
        parts = line.split(None, 1)
        if not parts:
            return
        word = parts[0].upper()
        if word in asm.MNEMONICS or word in asm.VAR_MACROS or word == "FUNC":
            raise ValueError(
                f"Bare assembly instruction {parts[0]!r}; wrap it in an "
                f"'asm {{ ... }}' block or deploy with language='asm'"
            )

    def _parse_function(self, lines: List[str], i: int) -> int:
        match = re.match(r"function\s+(\w+)\s*\((.*?)\)", lines[i])
        if not match:
            raise ValueError(f"Invalid function header: {lines[i]!r}")

        name = match.group(1)
        params = [param.strip() for param in match.group(2).split(",") if param.strip()]

        body_label = self._new_label()
        skip_label = self._new_label()

        self._emit("PUSH", name)
        for param in params:
            self._emit("PUSH", param)
        self._emit("PUSH", len(params))
        self._emit("DEF_FUNC", body_label)
        self._emit("JMP", skip_label)

        self._label(body_label)
        i = self._parse_block(lines, i + 1)
        self._emit("END_FUNC")
        self._label(skip_label)
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
        self._emit("PUSH", var)
        self._emit("PUSH", start)
        self._emit("SET")

        self._label(check_label)
        self._emit("PUSH", var)
        self._emit("GET")
        self._emit("PUSH", end)
        self._emit("LT")
        self._emit("JZ", end_label)

        # `continue` jumps here: increment first, then re-check the condition.
        self.loop_stack.append((continue_label, end_label))
        i = self._parse_block(lines, i + 1)
        self.loop_stack.pop()

        self._label(continue_label)
        self._emit("PUSH", var)
        self._emit("PUSH", var)
        self._emit("GET")
        self._emit("PUSH", 1)
        self._emit("ADD")
        self._emit("SET")
        self._emit("JMP", check_label)
        self._label(end_label)
        return i

    def _parse_while(self, lines: List[str], i: int) -> int:
        continue_label = self._new_label()
        end_label = self._new_label()

        self._label(continue_label)
        self._compile_expression(self._tokenize_expression(self._extract_condition(lines[i])))
        self._emit("JZ", end_label)

        self.loop_stack.append((continue_label, end_label))
        i = self._parse_block(lines, i + 1)
        self.loop_stack.pop()

        self._emit("JMP", continue_label)
        self._label(end_label)
        return i

    def _parse_if(self, lines: List[str], i: int) -> int:
        end_label = self._new_label()

        i = self._parse_conditional_branch(lines, i, end_label)
        while i < len(lines) and lines[i].startswith("else if"):
            i = self._parse_conditional_branch(lines, i, end_label)

        if i < len(lines) and lines[i].startswith("else"):
            i = self._parse_block(lines, i + 1)

        self._label(end_label)
        return i

    def _parse_conditional_branch(self, lines: List[str], i: int, end_label: str) -> int:
        next_label = self._new_label()

        self._compile_expression(self._tokenize_expression(self._extract_condition(lines[i])))
        self._emit("JZ", next_label)

        i = self._parse_block(lines, i + 1)

        self._emit("JMP", end_label)
        self._label(next_label)
        return i

    # ------------------------------------------------------------------ #
    # Simple statements
    # ------------------------------------------------------------------ #

    def _parse_assignment(self, line: str):
        target, expr = map(str.strip, line.split("=", 1))

        subscript = re.match(r"^(\w+)\s*\[(.*)\]$", target)
        if subscript:
            name, index_expr = subscript.group(1), subscript.group(2)
            self._emit("PUSH", name)
            self._compile_expression(self._tokenize_expression(index_expr))
            self._compile_expression(self._tokenize_expression(expr))
            self._emit("DICT_SET")
            return

        self._emit("PUSH", target)
        self._compile_expression(self._tokenize_expression(expr))
        self._emit("SET")

    def _parse_struct_literal(self, line: str):
        # Example: mylist := [1, 2, 3]
        var, value = map(str.strip, line.split(":=", 1))

        if value.startswith("["):
            inner = value[1:value.rindex("]")] if value.endswith("]") else value[1:]
            items = self._split_arguments(inner)
            self._emit("PUSH", var)
            self._emit("PUSH", [])
            self._emit("SET")
            for item in items:
                self._emit("PUSH", var)
                self._compile_expression(self._tokenize_expression(item))
                self._emit("LIST_APPEND")
            return

        if value.startswith("{"):
            self._emit("PUSH", var)
            self._emit("PUSH", {})
            self._emit("SET")
            return

        self._parse_assignment(line.replace(":=", "=", 1))

    def _parse_print(self, line: str):
        match = re.match(r"print\((.*)\)", line)
        expr = match.group(1) if match else ""
        if expr.strip():
            self._compile_expression(self._tokenize_expression(expr))
        else:
            self._emit("PUSH", "")
        self._emit("PRINT")

    def _parse_return(self, line: str):
        expr = line[len("return"):].strip()
        if expr:
            self._compile_expression(self._tokenize_expression(expr))
        self._emit("RET")

    def _parse_function_call(self, line: str):
        inner = line[len("woof "):].strip()
        match = re.match(r"(\w+)\s*\((.*)\)", inner)
        if not match:
            raise ValueError(f"Invalid function call: {line!r}")

        name, args_raw = match.group(1), match.group(2)
        args = self._split_arguments(args_raw)

        for arg in args:
            self._compile_expression(self._tokenize_expression(arg))

        self._emit("PUSH", name)
        self._emit("PUSH", len(args))
        self._emit("CALL_FUNC")

    # ------------------------------------------------------------------ #
    # Loop control
    # ------------------------------------------------------------------ #

    def _emit_break(self):
        if not self.loop_stack:
            raise ValueError("'break' used outside of a loop")
        _, end_label = self.loop_stack[-1]
        self._emit("JMP", end_label)

    def _emit_continue(self):
        if not self.loop_stack:
            raise ValueError("'continue' used outside of a loop")
        continue_label, _ = self.loop_stack[-1]
        self._emit("JMP", continue_label)

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
            self._emit("OR")

    def _parse_and(self):
        self._parse_comparison()
        while self._peek() == "&&":
            self._next()
            self._parse_comparison()
            self._emit("AND")

    def _parse_comparison(self):
        self._parse_additive()
        while self._peek() in COMPARISON_OPERATORS:
            operator = self._next()
            self._parse_additive()
            self._emit(BINARY_MNEMONICS[operator])

    def _parse_additive(self):
        self._parse_multiplicative()
        while self._peek() in ("+", "-"):
            operator = self._next()
            self._parse_multiplicative()
            self._emit(BINARY_MNEMONICS[operator])

    def _parse_multiplicative(self):
        self._parse_unary()
        while self._peek() in ("*", "/", "%"):
            operator = self._next()
            self._parse_unary()
            self._emit(BINARY_MNEMONICS[operator])

    def _parse_unary(self):
        if self._peek() == "-":
            self._next()
            self._emit("PUSH", 0)
            self._parse_unary()
            self._emit("SUB")
            return
        if self._peek() == "!":
            self._next()
            self._parse_unary()
            self._emit("PUSH", 0)
            self._emit("EQ")
            return
        self._parse_primary()

    def _parse_primary(self):
        token = self._next()
        if token is None:
            raise ValueError("Unexpected end of expression")

        if token.startswith('"'):
            self._emit("PUSH", ast.literal_eval(token))
            return

        if self._is_number(token):
            value = int(token) if "." not in token else float(token)
            self._emit("PUSH", value)
            return

        if token.isidentifier():
            if self._peek() == "[":
                self._next()  # consume '['
                self._emit("PUSH", token)
                self._parse_or()
                closing = self._next()
                if closing != "]":
                    raise ValueError(f"Expected ']' but found {closing!r}")
                self._emit("DICT_GET")
                return

            if self._peek() == "(":
                self._parse_call_arguments(token)
                return

            self._emit("PUSH", token)
            self._emit("GET")
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

        self._emit("PUSH", name)
        self._emit("PUSH", arg_count)
        self._emit("CALL_FUNC")

    def _tokenize_expression(self, expr: str) -> List[str]:
        return TOKEN_RE.findall(expr)

    # ------------------------------------------------------------------ #
    # Argument splitting
    # ------------------------------------------------------------------ #

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


def compile_pena(source: str, language: str | None = None) -> List[Any]:
    """Compile PENA or PENA Assembly source into SANVM bytecode.

    ``language`` may be ``"pena"`` (high level), ``"asm"`` (assembly) or
    ``None`` for auto-detection: clearly assembly-looking sources are
    assembled, everything else goes through the high-level compiler.
    """
    if language is None:
        selected = "asm" if looks_like_assembly(source) else "pena"
    elif isinstance(language, str):
        selected = language.strip().lower()
    else:
        raise ValueError(f"Unknown language: {language!r}")

    if selected in ("asm", "assembly", "pasm"):
        return assemble(source)
    if selected in ("pena", "hl", "high-level"):
        return PenaParser().parse(source)
    raise ValueError(f"Unknown language: {language!r}")
