"""PENA Assembly tests: assembler, disassembler, macros and inline asm."""
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

from SANVM.asm import (  # noqa: E402
    AsmError,
    assemble,
    disassemble,
    looks_like_assembly,
)
from SANVM.OpCode import OpCode  # noqa: E402
from SANVM.pena_parser import PenaParser, compile_pena  # noqa: E402
from SANVM.Storage import Storage  # noqa: E402
from SANVM.VM import SANVirtualMachine  # noqa: E402


def execute(source, language=None, storage=None, **kwargs):
    vm = SANVirtualMachine(storage if storage is not None else Storage(), **kwargs)
    vm.run(compile_pena(source, language))
    return vm


ALL_OPCODES = """
PUSH 1
POP
DROP
ADD
SUB
MUL
DIV
MOD
PRINT
JMP .end
JZ .end
JNZ .end
DUP
SWAP
OVER
ROT
AND
OR
XOR
EQ
NEQ
LT
LTE
GT
GTE
CALL
RET
NOP
SET
GET
DELETE
HAS
LIST_APPEND
LIST_REMOVE
LIST_LEN
LIST_GET
DICT_SET
DICT_GET
DICT_KEYS
DEF_FUNC .end
CALL_FUNC
END_FUNC
HALT
.end:
"""

LOOP_SUM = """
PUSH 0
SET total
PUSH 1
SET n
.loop:
GET n
PUSH 6
LT
JZ .done
GET total
GET n
ADD
SET total
GET n
PUSH 1
ADD
SET n
JMP .loop
.done:
GET total
PRINT
HALT
"""

FUNC_ADD = """
FUNC add(a, b) {
  GET a
  GET b
  ADD
  RET
}
PUSH 10
PUSH 20
PUSH add
PUSH 2
CALL_FUNC
PRINT
HALT
"""

COLLECTIONS = """
PUSH []
SET items
PUSH 1
LIST_APPEND items
PUSH 2
LIST_APPEND items
PUSH 3
LIST_APPEND items
LIST_LEN items
PRINT
PUSH 1
LIST_GET items
PRINT
PUSH 2
LIST_REMOVE items
LIST_LEN items
PRINT
PUSH {}
SET book
PUSH gold
PUSH 7
DICT_SET book
PUSH gold
DICT_GET book
PRINT
DICT_KEYS book
PRINT
HALT
"""

INLINE_ASM = """
x = 41
asm {
  GET x
  PUSH 1
  ADD
  SET y
}
print(y)
"""

LABEL_ISOLATION = """
asm {
  PUSH 0
  SET a
  .loop:
  GET a
  PUSH 2
  LT
  JZ .done
  GET a
  PUSH 1
  ADD
  SET a
  JMP .loop
  .done:
}
print(a)
asm {
  PUSH 5
  SET a
  .loop:
  GET a
  PUSH 7
  LT
  JZ .done
  GET a
  PUSH 1
  ADD
  SET a
  JMP .loop
  .done:
}
print(a)
"""


# ---------------------------------------------------------------------- #
# Assembler coverage
# ---------------------------------------------------------------------- #

def test_every_opcode_has_a_mnemonic_and_roundtrips():
    bytecode = assemble(ALL_OPCODES)
    integers = [item for item in bytecode if isinstance(item, int)]
    for opcode in OpCode:
        assert opcode.value in integers, f"missing opcode in assembly output: {opcode.name}"

    text = disassemble(bytecode)
    for opcode in OpCode:
        assert opcode.name in text, f"{opcode.name} missing from disassembly"
    assert assemble(text) == bytecode


def test_literals_and_symbols():
    vm = execute('PUSH "text"\nPUSH [1, "two", {"k": 3}]\nPUSH -4\nPUSH 0xFF\nPUSH true\nHALT')
    assert vm.stack == ["text", [1, "two", {"k": 3}], -4, 255, 1]

    vm = execute("PUSH greet\nHALT\n")
    assert vm.stack == ["greet"]


def test_labels_and_jumps():
    vm = execute(LOOP_SUM)
    assert [log["value"] for log in vm.logs] == [15]


def test_func_blocks_define_and_call_functions():
    vm = execute(FUNC_ADD)
    assert [log["value"] for log in vm.logs] == [30]


def test_variable_macros():
    source = """
    PUSH 5
    SET x
    GET x
    PRINT
    HAS x
    PRINT
    DELETE x
    HAS x
    PRINT
    HALT
    """
    vm = execute(source)
    assert [log["value"] for log in vm.logs] == [5, 1, 0]


def test_collection_macros():
    vm = execute(COLLECTIONS)
    assert [log["value"] for log in vm.logs] == [3, 2, 2, 7, ["gold"]]


# ---------------------------------------------------------------------- #
# Inline assembly and language detection
# ---------------------------------------------------------------------- #

def test_inline_asm_inside_high_level_program():
    vm = execute(INLINE_ASM)
    assert [log["value"] for log in vm.logs] == [42]


def test_inline_asm_label_isolation():
    vm = execute(LABEL_ISOLATION)
    assert [log["value"] for log in vm.logs] == [2, 7]


def test_inline_asm_wrapper_is_a_full_program():
    source = """
    asm {
      PUSH 20
      PUSH 22
      ADD
      PRINT
    }
    """
    vm = execute(source)
    assert [log["value"] for log in vm.logs] == [42]


def test_language_detection():
    assert looks_like_assembly("PUSH 1\nPRINT\nHALT")
    assert looks_like_assembly("; comment\n.loop:\nJMP .loop")
    assert looks_like_assembly("FUNC add(a, b) {\nRET\n}")
    assert looks_like_assembly("set x") is False  # lower case needs explicit language
    assert looks_like_assembly("print(1)") is False
    assert looks_like_assembly("x = 1") is False
    assert looks_like_assembly("function f() {\n return 1\n}") is False
    assert looks_like_assembly("asm {\nPUSH 1\n}") is False

    assert compile_pena("PUSH 1\nPRINT\nHALT") == assemble("PUSH 1\nPRINT\nHALT")
    assert compile_pena("push 1\nprint\nhalt", "asm") == assemble("PUSH 1\nPRINT\nHALT")
    assert compile_pena("x = 1\nprint(x)") == PenaParser().parse("x = 1\nprint(x)")


def test_bare_assembly_is_rejected_by_the_high_level_parser():
    with pytest.raises(ValueError, match="Bare assembly"):
        PenaParser().parse("PUSH 1\nPRINT\nHALT")


def test_high_level_still_compiles_through_the_same_encoder():
    source = 'x = (3 + 5) * 2 - 4 / 2 % 3\nprint(x)'
    bytecode = PenaParser().parse(source)
    assert bytecode == compile_pena(source)
    vm = SANVirtualMachine(Storage())
    vm.run(bytecode)
    assert [log["value"] for log in vm.logs] == [14]


def test_high_level_and_assembly_parity():
    high_level = "function add(a, b) {\n return a + b\n}\nprint(add(2, 3))"
    assembly = """
    FUNC add(a, b) {
      GET a
      GET b
      ADD
      RET
    }
    PUSH 2
    PUSH 3
    PUSH add
    PUSH 2
    CALL_FUNC
    PRINT
    HALT
    """
    reference = SANVirtualMachine(Storage())
    reference.run(PenaParser().parse(high_level))
    candidate = SANVirtualMachine(Storage())
    candidate.run(assemble(assembly))
    assert candidate.logs == reference.logs


# ---------------------------------------------------------------------- #
# Disassembler
# ---------------------------------------------------------------------- #

CORPUS = [
    LOOP_SUM,
    FUNC_ADD,
    COLLECTIONS,
    'x = (3 + 5) * 2 - 4 / 2 % 3\nprint(x)',
    'mylist := [1, 2, 3]\nprint(mylist[1])\nprint(mylist)',
    'x = 1\nif (x == 1 && x != 2) {\n print("ok")\n}',
    'function fib(n) {\n if (n < 2) {\n  return n\n }\n return fib(n - 1) + fib(n - 2)\n}\nprint(fib(6))',
]


@pytest.mark.parametrize("source", CORPUS)
def test_disassemble_roundtrip(source):
    bytecode = compile_pena(source)
    assert assemble(disassemble(bytecode)) == bytecode


def test_disassemble_empty_program():
    assert disassemble([]) == ""
    assert assemble("") == []


# ---------------------------------------------------------------------- #
# Errors
# ---------------------------------------------------------------------- #

def test_assembler_errors():
    with pytest.raises(AsmError):
        assemble("BOGUS 1")
    with pytest.raises(AsmError):
        assemble("PUSH")
    with pytest.raises(AsmError):
        assemble('PUSH "unterminated')
    with pytest.raises(AsmError):
        assemble("JMP .nowhere")
    with pytest.raises(AsmError):
        assemble(".x:\n.x:")
    with pytest.raises(AsmError):
        assemble("SET 5")
    with pytest.raises(AsmError):
        assemble("ADD 1")
    with pytest.raises(AsmError):
        assemble("PUSH 1\n}")
    with pytest.raises(AsmError):
        assemble("FUNC f() {\nPUSH 1")
    with pytest.raises(AsmError):
        disassemble([0x99])


def test_high_level_errors():
    with pytest.raises(ValueError, match="Unterminated asm block"):
        PenaParser().parse("asm {\nPUSH 1")
    with pytest.raises(ValueError, match="Unknown language"):
        compile_pena("x = 1", "cobol")
