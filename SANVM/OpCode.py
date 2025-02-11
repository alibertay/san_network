from enum import Enum

class OpCode(Enum):
    PUSH = 0x01
    POP = 0x02
    ADD = 0x03
    SUB = 0x04
    MUL = 0x05
    DIV = 0x06
    PRINT = 0x07
    MOD = 0x08
    JMP = 0x09
    IF = 0x0A
    DUP = 0x0B
    SWAP = 0x0C
    AND = 0x0D
    OR = 0x0E
    XOR = 0x0F
    EQ = 0x11
    NEQ = 0x12
    LT = 0x13
    LTE = 0x14
    GT = 0x15
    GTE = 0x16
    CALL = 0x17
    RET = 0x18
    NOP = 0x19
    DROP = 0x1A
    OVER = 0x1B
    ROT = 0x1C
    SET = 0x1D
    GET = 0x1E
    DELETE = 0x1F
    HAS = 0x20
    LIST_APPEND = 0x21
    LIST_REMOVE = 0x22
    LIST_LEN = 0x23
    LIST_GET = 0x24
    DICT_SET = 0x25
    DICT_GET = 0x26
    DICT_KEYS = 0x27
    FOR_LOOP = 0x28
    BREAK_LOOP = 0x29
    CONTINUE_LOOP = 0x2A
    DEF_FUNC = 0x2B
    CALL_FUNC = 0x2C
    HALT = 0xFF