"""Deterministic gas schedule for the SAN virtual machine.

Every opcode has a fixed cost so that a transaction's gas usage is identical
on every node. Costs are deliberately simple: compute is cheap, state access
is expensive.
"""

from __future__ import annotations

from typing import Any

from SANVM.OpCode import OpCode

DEFAULT_COST = 1
PUSH_PAYLOAD_BYTES_PER_GAS = 16

GAS_COSTS: dict[int, int] = {
    OpCode.PUSH.value: 1,
    OpCode.POP.value: 1,
    OpCode.DROP.value: 1,
    OpCode.ADD.value: 1,
    OpCode.SUB.value: 1,
    OpCode.MUL.value: 2,
    OpCode.DIV.value: 3,
    OpCode.MOD.value: 3,
    OpCode.PRINT.value: 10,
    OpCode.JMP.value: 2,
    OpCode.JZ.value: 2,
    OpCode.JNZ.value: 2,
    OpCode.NOP.value: 0,
    OpCode.HALT.value: 0,
    OpCode.DUP.value: 1,
    OpCode.SWAP.value: 1,
    OpCode.OVER.value: 1,
    OpCode.ROT.value: 1,
    OpCode.AND.value: 1,
    OpCode.OR.value: 1,
    OpCode.XOR.value: 1,
    OpCode.EQ.value: 1,
    OpCode.NEQ.value: 1,
    OpCode.LT.value: 1,
    OpCode.LTE.value: 1,
    OpCode.GT.value: 1,
    OpCode.GTE.value: 1,
    OpCode.CALL.value: 5,
    OpCode.RET.value: 5,
    OpCode.END_FUNC.value: 5,
    OpCode.DEF_FUNC.value: 5,
    OpCode.CALL_FUNC.value: 10,
    # State access
    OpCode.SET.value: 20,
    OpCode.GET.value: 10,
    OpCode.DELETE.value: 20,
    OpCode.HAS.value: 10,
    OpCode.LIST_APPEND.value: 25,
    OpCode.LIST_REMOVE.value: 25,
    OpCode.LIST_LEN.value: 10,
    OpCode.LIST_GET.value: 10,
    OpCode.DICT_SET.value: 25,
    OpCode.DICT_GET.value: 10,
    OpCode.DICT_KEYS.value: 15,
}


def payload_cost(value: Any) -> int:
    """PUSH payloads are charged by serialized size."""
    from utils import canonical

    try:
        size = len(canonical.dumps_bytes(value))
    except (TypeError, ValueError):
        size = len(str(value))
    return size // PUSH_PAYLOAD_BYTES_PER_GAS


def instruction_cost(opcode: int) -> int:
    return GAS_COSTS.get(opcode, DEFAULT_COST)
