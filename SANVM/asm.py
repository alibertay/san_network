"""PENA Assembly (PASM): textual instruction set, assembler and disassembler.

Assembly is the canonical instruction layer of the SANVM. High-level PENA is
lowered to assembly instructions by :class:`SANVM.pena_parser.PenaParser`;
pure assembly contracts are compiled by :func:`assemble`. Both paths share the
same encoder and therefore produce identical flat bytecode consumed by
:class:`SANVM.VM.SANVirtualMachine`.

Syntax
------
* one instruction per line: ``MNEMONIC [operand]``
* labels: ``name:`` (an instruction may follow on the same line)
* comments: ``;`` or ``//`` (outside string literals)
* literals: integers (decimal, ``0x``, ``0b``), floats, strings, lists, dicts
* bare identifiers are symbols (string values): ``PUSH greet`` == ``PUSH "greet"``
* ``true`` / ``false`` are the integers ``1`` / ``0``
* function blocks::

      FUNC add(a, b) {
          GET a
          GET b
          ADD
          RET
      }

* variable macros (operand optional; without it the raw opcode is emitted):
  ``GET name``, ``HAS name``, ``DELETE name``, ``LIST_LEN name``,
  ``DICT_KEYS name`` expand to ``PUSH "name"`` + opcode; ``SET name``,
  ``LIST_APPEND name``, ``LIST_REMOVE name``, ``LIST_GET name``,
  ``DICT_GET name`` insert a ``SWAP`` so the pending value/index stays on top;
  ``DICT_SET name`` inserts two ``ROT``s (three pending operands)
* jump targets are labels (forward references allowed) or absolute addresses
"""

from __future__ import annotations

import ast
import re
from typing import Any, Dict, Iterable, List, Optional, Sequence, Tuple

from SANVM.OpCode import OpCode

Instruction = Tuple[str, List[Any]]

#: Record name used for label definitions inside the instruction list.
LABEL = "__label__"

MAX_LITERAL_CHARS = 65_536

MNEMONICS: Dict[str, int] = {op.name: op.value for op in OpCode}
OPCODES_BY_VALUE: Dict[int, str] = {op.value: op.name for op in OpCode}

TARGET_MNEMONICS = frozenset({"JMP", "JZ", "JNZ", "DEF_FUNC"})

#: Macros that accept a variable name and expand to PUSH + opcode. The value
#: is the number of pending stack operands the opcode consumes *including* the
#: variable name that is being inserted underneath them.
VAR_MACROS: Dict[str, int] = {
    "GET": 1,
    "HAS": 1,
    "DELETE": 1,
    "LIST_LEN": 1,
    "DICT_KEYS": 1,
    "SET": 2,
    "LIST_APPEND": 2,
    "LIST_REMOVE": 2,
    "LIST_GET": 2,
    "DICT_GET": 2,
    "DICT_SET": 3,
}

_SYMBOL_RE = re.compile(r"[A-Za-z_$][\w$]*")
_LABEL_RE = re.compile(r"[A-Za-z_.$][\w.$]*")
_ADDRESS_RE = re.compile(r"-?\d+|0[xX][0-9a-fA-F]+|0[bB][01]+")
_FUNC_RE = re.compile(
    r"^FUNC\s+([A-Za-z_$][\w$]*)\s*(?:\(([^)]*)\))?\s*\{\s*$", re.IGNORECASE
)
_BARE_ASM_RE = re.compile(r"(?m)^[ \t]*asm[ \t]*\{")


class AsmError(ValueError):
    """Deterministic assembly failure (bad mnemonic, operand, label, ...)."""


# ---------------------------------------------------------------------- #
# Source helpers
# ---------------------------------------------------------------------- #

def strip_comment(line: str) -> str:
    """Remove a ``;`` or ``//`` comment, ignoring string literals."""
    out: List[str] = []
    in_string = False
    escape = False
    i = 0
    while i < len(line):
        char = line[i]
        if in_string:
            out.append(char)
            if escape:
                escape = False
            elif char == "\\":
                escape = True
            elif char == '"':
                in_string = False
            i += 1
            continue
        if char == '"':
            in_string = True
            out.append(char)
        elif char == ";":
            break
        elif char == "/" and i + 1 < len(line) and line[i + 1] == "/":
            break
        else:
            out.append(char)
        i += 1
    return "".join(out)


def _brace_delta(text: str) -> int:
    delta = 0
    in_string = False
    escape = False
    for char in text:
        if in_string:
            if escape:
                escape = False
            elif char == "\\":
                escape = True
            elif char == '"':
                in_string = False
            continue
        if char == '"':
            in_string = True
        elif char == "{":
            delta += 1
        elif char == "}":
            delta -= 1
    return delta


def matching_brace(text: str, open_index: int) -> int:
    depth = 0
    in_string = False
    escape = False
    for i in range(open_index, len(text)):
        char = text[i]
        if in_string:
            if escape:
                escape = False
            elif char == "\\":
                escape = True
            elif char == '"':
                in_string = False
            continue
        if char == '"':
            in_string = True
        elif char == "{":
            depth += 1
        elif char == "}":
            depth -= 1
            if depth == 0:
                return i
    return -1


def strip_outer_wrapper(source: str) -> str:
    """Return the body of ``asm { ... }`` when the whole source is wrapped."""
    match = _BARE_ASM_RE.search(source)
    if match is None:
        return source
    prefix = source[: match.start()]
    if any(strip_comment(line).strip() for line in prefix.splitlines()):
        return source
    brace_index = source.index("{", match.start())
    close = matching_brace(source, brace_index)
    if close == -1:
        raise AsmError("Unterminated asm block")
    tail = strip_comment(source[close + 1:]).strip()
    if tail:
        raise AsmError(f"Unexpected text after asm block: {tail!r}")
    return source[brace_index + 1: close]


# ---------------------------------------------------------------------- #
# Pure assembly detection
# ---------------------------------------------------------------------- #

def looks_like_assembly(source: str) -> bool:
    """True when the first meaningful line is clearly PASM code.

    Detection is intentionally conservative: mnemonics/macros must be written
    in upper case and labels must be the first token of the line. This keeps
    high-level PENA sources (``print(x)``, ``function f() {``) unambiguous.
    """
    for raw in source.splitlines():
        text = strip_comment(raw).strip()
        if not text:
            continue
        if re.match(r"^asm\b", text):
            return False
        label_match = re.match(r"^([^\s:]+)\s*:(?!=)", text)
        if label_match:
            head = label_match.group(1)
            upper = head.upper()
            if upper not in MNEMONICS and upper not in VAR_MACROS \
                    and upper != "FUNC" and _LABEL_RE.fullmatch(head):
                return True
            return False
        token_match = re.match(r"^[A-Za-z_]\w*", text)
        if token_match is None:
            return False
        return token_match.group(0) in MNEMONICS \
            or token_match.group(0) in VAR_MACROS \
            or token_match.group(0) == "FUNC"
    return False


# ---------------------------------------------------------------------- #
# Operand parsing
# ---------------------------------------------------------------------- #

def parse_literal(text: str, mnemonic: str) -> Any:
    """Parse a PUSH operand: symbol, ``true``/``false`` or Python literal."""
    if not text:
        raise AsmError(f"{mnemonic} requires an operand")
    if text == "true":
        return 1
    if text == "false":
        return 0
    if _SYMBOL_RE.fullmatch(text):
        return text
    if len(text) > MAX_LITERAL_CHARS:
        raise AsmError(f"{mnemonic} literal is too large ({len(text)} chars)")
    try:
        value = ast.literal_eval(text)
    except (ValueError, SyntaxError, MemoryError) as exc:
        raise AsmError(f"Invalid literal for {mnemonic}: {text!r}") from exc
    if isinstance(value, (bytes, bytearray, set, frozenset, tuple, complex, range)):
        raise AsmError(f"Unsupported literal for {mnemonic}: {text!r}")
    return value


def parse_symbol(text: str, mnemonic: str) -> str:
    """Parse a variable/symbol operand: ``name`` or ``"name"``."""
    if not text:
        raise AsmError(f"{mnemonic} requires a variable name")
    if _SYMBOL_RE.fullmatch(text):
        return text
    if len(text) > MAX_LITERAL_CHARS:
        raise AsmError(f"{mnemonic} operand is too large")
    try:
        value = ast.literal_eval(text)
    except (ValueError, SyntaxError, MemoryError) as exc:
        raise AsmError(f"Invalid variable name for {mnemonic}: {text!r}") from exc
    if not isinstance(value, str):
        raise AsmError(f"{mnemonic} expects a variable name, got {text!r}")
    return value


def parse_target(text: str, mnemonic: str, namespace: str) -> Any:
    """Parse a jump/function target: label name or absolute address."""
    if _ADDRESS_RE.fullmatch(text):
        try:
            return int(ast.literal_eval(text))
        except (ValueError, SyntaxError) as exc:
            raise AsmError(f"Invalid address for {mnemonic}: {text!r}") from exc
    if _LABEL_RE.fullmatch(text):
        return namespace + text
    raise AsmError(f"{mnemonic} expects a label or address, got {text!r}")


# ---------------------------------------------------------------------- #
# Assembler
# ---------------------------------------------------------------------- #

class _Assembler:
    def __init__(self, namespace: str = ""):
        self.namespace = namespace
        self.instructions: List[Instruction] = []
        self._func_counter = 0

    def parse(self, lines: Sequence[str]) -> List[Instruction]:
        self._parse_lines(list(lines))
        return self.instructions

    # -- line loop ----------------------------------------------------- #

    def _parse_lines(self, lines: List[str]) -> None:
        i = 0
        while i < len(lines):
            text = strip_comment(lines[i]).strip()
            i += 1
            if not text:
                continue
            if text == "}":
                raise AsmError("Unexpected '}': not inside a FUNC block")

            func_match = _FUNC_RE.match(text)
            if func_match:
                body: List[str] = []
                depth = 1
                while i < len(lines):
                    raw = strip_comment(lines[i])
                    depth += _brace_delta(raw)
                    i += 1
                    if depth <= 0:
                        break
                    body.append(raw)
                else:
                    raise AsmError(f"Unterminated FUNC block: {text!r}")
                self._emit_function(func_match.group(1), func_match.group(2), body)
                continue

            label, rest = _split_label(text)
            if label is not None:
                self._label(self.namespace + label)
                if rest:
                    self._compile(rest)
                continue

            self._compile(text)

    def _compile(self, text: str) -> None:
        parts = text.split(None, 1)
        mnemonic = parts[0].upper()
        operand = parts[1].strip() if len(parts) > 1 else ""

        if mnemonic == "PUSH":
            self.instructions.append(("PUSH", [parse_literal(operand, "PUSH")]))
            return

        if mnemonic in TARGET_MNEMONICS:
            target = parse_target(operand, mnemonic, self.namespace)
            self.instructions.append((mnemonic, [target]))
            return

        if mnemonic in VAR_MACROS:
            if operand:
                symbol = parse_symbol(operand, mnemonic)
                self.instructions.append(("PUSH", [symbol]))
                arity = VAR_MACROS[mnemonic]
                if arity == 2:
                    self.instructions.append(("SWAP", []))
                elif arity == 3:
                    self.instructions.append(("ROT", []))
                    self.instructions.append(("ROT", []))
                self.instructions.append((mnemonic, []))
                return

        if mnemonic in MNEMONICS:
            if operand:
                raise AsmError(f"{mnemonic} does not take operands: {text!r}")
            self.instructions.append((mnemonic, []))
            return

        raise AsmError(f"Unknown mnemonic: {parts[0]!r}")

    def _emit_function(self, name: str, params_raw: Optional[str],
                       body: List[str]) -> None:
        params = [param.strip() for param in (params_raw or "").split(",") if param.strip()]
        for param in params:
            if not _SYMBOL_RE.fullmatch(param):
                raise AsmError(f"Invalid parameter name: {param!r}")

        self._func_counter += 1
        body_label = f"{self.namespace}__func_{self._func_counter}__body"
        skip_label = f"{self.namespace}__func_{self._func_counter}__skip"

        self.instructions.append(("PUSH", [name]))
        for param in params:
            self.instructions.append(("PUSH", [param]))
        self.instructions.append(("PUSH", [len(params)]))
        self.instructions.append(("DEF_FUNC", [body_label]))
        self.instructions.append(("JMP", [skip_label]))
        self._label(body_label)
        self._parse_lines(body)
        self.instructions.append(("END_FUNC", []))
        self._label(skip_label)

    def _label(self, name: str) -> None:
        self.instructions.append((LABEL, [name]))


def _split_label(text: str) -> Tuple[Optional[str], str]:
    if ":" not in text:
        return None, text
    head, _, tail = text.partition(":")
    head = head.strip()
    if not head or not _LABEL_RE.fullmatch(head):
        return None, text
    if tail.startswith("="):
        # ``name := ...`` is an assignment, not a label
        return None, text
    upper = head.upper()
    if upper in MNEMONICS or upper in VAR_MACROS or upper == "FUNC":
        return None, text
    return head, tail.strip()


def parse_lines(lines: Sequence[str], namespace: str = "") -> List[Instruction]:
    """Expand an assembly program into ``(mnemonic, operands)`` records."""
    return _Assembler(namespace).parse(lines)


def parse_source(source: str, namespace: str = "") -> List[Instruction]:
    return parse_lines(source.splitlines(), namespace)


# ---------------------------------------------------------------------- #
# Encoder
# ---------------------------------------------------------------------- #

def _instruction_size(mnemonic: str) -> int:
    return 2 if mnemonic == "PUSH" or mnemonic in TARGET_MNEMONICS else 1


def encode(instructions: Iterable[Instruction]) -> List[Any]:
    """Resolve labels and encode assembly records into SANVM bytecode."""
    items = list(instructions)

    labels: Dict[str, int] = {}
    position = 0
    for name, operands in items:
        if name == LABEL:
            label_name = operands[0]
            if label_name in labels:
                raise AsmError(f"Duplicate label: {label_name}")
            labels[label_name] = position
            continue
        if name not in MNEMONICS:
            raise AsmError(f"Unknown mnemonic: {name!r}")
        position += _instruction_size(name)

    total = position
    bytecode: List[Any] = []
    for name, operands in items:
        if name == LABEL:
            continue
        bytecode.append(MNEMONICS[name])
        if name == "PUSH":
            bytecode.append(operands[0])
        elif name in TARGET_MNEMONICS:
            target = operands[0]
            if isinstance(target, str):
                if target not in labels:
                    raise AsmError(f"Unknown label: {target}")
                target = labels[target]
            if isinstance(target, bool) or not isinstance(target, int):
                raise AsmError(f"Invalid target: {target!r}")
            if not 0 <= target <= total:
                raise AsmError(f"Target out of range: {target!r}")
            bytecode.append(target)
    return bytecode


def assemble_lines(lines: Sequence[str], namespace: str = "") -> List[Any]:
    return encode(parse_lines(lines, namespace))


def assemble(source: str, namespace: str = "") -> List[Any]:
    return encode(parse_source(strip_outer_wrapper(source), namespace))


# ---------------------------------------------------------------------- #
# Disassembler
# ---------------------------------------------------------------------- #

def format_literal(value: Any) -> str:
    if isinstance(value, int) and not isinstance(value, bool):
        return str(value)
    return repr(value)


def disassemble(bytecode: Sequence[Any]) -> str:
    """Render bytecode as PASM text (jump targets become local labels)."""
    items = list(bytecode)

    targets: set[int] = set()
    i = 0
    while i < len(items):
        opcode = items[i]
        if not isinstance(opcode, int) or opcode not in OPCODES_BY_VALUE:
            i += 1
            continue
        name = OPCODES_BY_VALUE[opcode]
        if name == "PUSH":
            i += 2
            continue
        if name in TARGET_MNEMONICS:
            if i + 1 >= len(items) or not isinstance(items[i + 1], int):
                raise AsmError(f"Truncated {name} instruction at position {i}")
            targets.add(items[i + 1])
            i += 2
            continue
        i += 1

    lines: List[str] = []
    i = 0
    while i < len(items):
        if i in targets:
            lines.append(f".L{i}:")
        opcode = items[i]
        if not isinstance(opcode, int) or opcode not in OPCODES_BY_VALUE:
            raise AsmError(f"Invalid opcode at position {i}: {opcode!r}")
        name = OPCODES_BY_VALUE[opcode]
        if name == "PUSH":
            if i + 1 >= len(items):
                raise AsmError(f"Truncated PUSH instruction at position {i}")
            lines.append(f"PUSH {format_literal(items[i + 1])}")
            i += 2
            continue
        if name in TARGET_MNEMONICS:
            if i + 1 >= len(items) or not isinstance(items[i + 1], int):
                raise AsmError(f"Truncated {name} instruction at position {i}")
            lines.append(f"{name} .L{items[i + 1]}")
            i += 2
            continue
        lines.append(name)
        i += 1

    if len(items) in targets:
        lines.append(f".L{len(items)}:")
    return "\n".join(lines)


__all__ = [
    "AsmError",
    "Instruction",
    "LABEL",
    "MNEMONICS",
    "OPCODES_BY_VALUE",
    "TARGET_MNEMONICS",
    "VAR_MACROS",
    "assemble",
    "assemble_lines",
    "disassemble",
    "encode",
    "format_literal",
    "looks_like_assembly",
    "matching_brace",
    "parse_lines",
    "parse_source",
    "strip_comment",
    "strip_outer_wrapper",
]
