import copy
import logging

from SANVM.ContractManager import ContractManager
from SANVM.gas import instruction_cost, payload_cost
from SANVM.OpCode import OpCode
from SANVM.Storage import Storage
from utils import canonical

logger = logging.getLogger(__name__)


class VMError(Exception):
    """Deterministic VM failure (bad opcode, limits, invalid operand, ...)."""


class StepLimitExceeded(VMError):
    pass


class StackLimitExceeded(VMError):
    pass


class OutOfGas(VMError):
    pass


class SANVirtualMachine:
    DEFAULT_MAX_STEPS = 100_000
    DEFAULT_MAX_STACK = 1024
    DEFAULT_MAX_CALL_DEPTH = 64
    # Audit fix: a single value can never exceed this many serialized bytes,
    # and growing storage is charged by the size of the value.
    MAX_VALUE_BYTES = 65_536
    # Audit fix: integers are capped and arithmetic is priced by operand size,
    # so repeated squaring cannot burn CPU/RAM for 2 gas.
    MAX_INT_BITS = 4096

    def __init__(self, storage=None, max_steps=None, max_stack=None,
                 max_call_depth=None, gas_limit=None, verbose=True):
        self.storage = storage if storage else Storage()
        self.contract_manager = ContractManager(self.storage)

        self.max_steps = max_steps or self.DEFAULT_MAX_STEPS
        self.max_stack = max_stack or self.DEFAULT_MAX_STACK
        self.max_call_depth = max_call_depth or self.DEFAULT_MAX_CALL_DEPTH
        self.gas_limit = gas_limit
        self.gas_used = 0
        self.verbose = verbose
        self.logs: list[dict] = []

        self.stack = []
        self.call_stack = []
        self.scopes = []
        self.running = True
        self.pc = 0
        self.steps = 0
        self.bytecode = []

        self.instructions = {
            OpCode.PUSH.value: self.push,
            OpCode.POP.value: self.pop,
            OpCode.DROP.value: self.drop,
            OpCode.ADD.value: self.add,
            OpCode.SUB.value: self.sub,
            OpCode.MUL.value: self.mul,
            OpCode.DIV.value: self.div,
            OpCode.MOD.value: self.mod,
            OpCode.PRINT.value: self.print_top,
            OpCode.HALT.value: self.halt,
            OpCode.JMP.value: self.jmp,
            OpCode.JZ.value: self.jz,
            OpCode.JNZ.value: self.jnz,
            OpCode.DUP.value: self.dup,
            OpCode.SWAP.value: self.swap,
            OpCode.OVER.value: self.over,
            OpCode.ROT.value: self.rot,
            OpCode.AND.value: self.AND,
            OpCode.OR.value: self.OR,
            OpCode.XOR.value: self.XOR,
            OpCode.EQ.value: self.eq,
            OpCode.NEQ.value: self.neq,
            OpCode.LT.value: self.lt,
            OpCode.LTE.value: self.lte,
            OpCode.GT.value: self.gt,
            OpCode.GTE.value: self.gte,
            OpCode.CALL.value: self.call,
            OpCode.RET.value: self.ret,
            OpCode.NOP.value: self.nop,
            OpCode.SET.value: self.set_var,
            OpCode.GET.value: self.get_var,
            OpCode.DELETE.value: self.delete_var,
            OpCode.HAS.value: self.has_var,
            OpCode.LIST_APPEND.value: self.list_append,
            OpCode.LIST_REMOVE.value: self.list_remove,
            OpCode.LIST_LEN.value: self.list_len,
            OpCode.LIST_GET.value: self.list_get,
            OpCode.DICT_SET.value: self.dict_set,
            OpCode.DICT_GET.value: self.dict_get,
            OpCode.DICT_KEYS.value: self.dict_keys,
            OpCode.DEF_FUNC.value: self.define_function,
            OpCode.CALL_FUNC.value: self.call_function,
            OpCode.END_FUNC.value: self.end_function,
        }

    # ------------------------------------------------------------------ #
    # Execution loop
    # ------------------------------------------------------------------ #

    def run(self, bytecode, start_pc=0, gas_limit=None):
        """Execute ``bytecode`` starting at ``start_pc``.

        State is reset on every call; execution is bounded by ``max_steps`` and
        by the gas schedule so a hostile contract can never hang a node.
        ``gas_used`` is available after the call (also on ``OutOfGas``).
        """
        self.bytecode = list(bytecode)
        self.stack = []
        self.call_stack = []
        self.scopes = []
        self.running = True
        self.steps = 0
        self.pc = start_pc
        if gas_limit is not None:
            self.gas_limit = gas_limit
        self.gas_used = 0
        self.logs = []

        while self.running and 0 <= self.pc < len(self.bytecode):
            if self.steps >= self.max_steps:
                raise StepLimitExceeded(
                    f"Execution exceeded {self.max_steps} steps"
                )
            self.steps += 1

            opcode = self.bytecode[self.pc]
            self.pc += 1
            self._charge(instruction_cost(opcode) if isinstance(opcode, int) else 0)

            handler = self.instructions.get(opcode)
            if handler is None:
                raise VMError(f"Unknown opcode: {opcode!r} at pc {self.pc - 1}")

            handler()

    @staticmethod
    def _word_cost(a, b) -> int:
        if isinstance(a, int) and isinstance(b, int):
            return max(a.bit_length(), b.bit_length()) // 64
        return 0

    def _charge(self, cost: int) -> None:
        self.gas_used += cost
        if self.gas_limit is not None and self.gas_used > self.gas_limit:
            raise OutOfGas(
                f"Out of gas: used {self.gas_used}, limit {self.gas_limit}"
            )

    # ------------------------------------------------------------------ #
    # Helpers
    # ------------------------------------------------------------------ #

    def _push(self, value):
        if len(self.stack) >= self.max_stack:
            raise StackLimitExceeded(f"Stack exceeded {self.max_stack} items")
        if isinstance(value, (list, dict, set)):
            value = copy.deepcopy(value)
            if len(canonical.dumps_bytes(value)) > self.MAX_VALUE_BYTES:
                raise VMError(f"Value exceeds {self.MAX_VALUE_BYTES} bytes")
        elif isinstance(value, str) and len(value.encode("utf-8")) > self.MAX_VALUE_BYTES:
            raise VMError(f"Value exceeds {self.MAX_VALUE_BYTES} bytes")
        elif (
            isinstance(value, int)
            and not isinstance(value, bool)
            and value.bit_length() > self.MAX_INT_BITS
        ):
            raise VMError(f"Integer exceeds {self.MAX_INT_BITS} bits")
        self.stack.append(value)

    def _read_target(self) -> int:
        if self.pc >= len(self.bytecode):
            raise VMError("Jump/function target is missing")
        target = self.bytecode[self.pc]
        self.pc += 1
        if not isinstance(target, int) or not (0 <= target <= len(self.bytecode)):
            raise VMError(f"Invalid jump target: {target!r}")
        return target

    def _require(self, count):
        while len(self.stack) < count:
            self._push(0)

    # ------------------------------------------------------------------ #
    # Stack and arithmetic
    # ------------------------------------------------------------------ #

    def push(self):
        if self.pc >= len(self.bytecode):
            raise VMError("PUSH without operand")
        value = self.bytecode[self.pc]
        self.pc += 1
        self._charge(payload_cost(value))
        self._push(value)

    def pop(self):
        if self.stack:
            self.stack.pop()

    def drop(self):
        if self.stack:
            self.stack.pop()

    def add(self):
        self._require(2)
        b = self.stack.pop()
        a = self.stack.pop()
        # Audit fix: price wide-integer arithmetic by operand size.
        self._charge(self._word_cost(a, b))
        self._push(a + b)

    def sub(self):
        self._require(2)
        b = self.stack.pop()
        a = self.stack.pop()
        self._charge(self._word_cost(a, b))
        self._push(a - b)

    def mul(self):
        self._require(2)
        b = self.stack.pop()
        a = self.stack.pop()
        # Audit fix: price big-integer multiplication by operand size.
        self._charge(self._word_cost(a, b))
        self._push(a * b)

    def div(self):
        self._require(2)
        b = self.stack.pop()
        a = self.stack.pop()

        if b == 0:
            raise VMError("Division by zero")

        self._charge(self._word_cost(a, b))
        self._push(a // b)

    def mod(self):
        self._require(2)
        b = self.stack.pop()
        a = self.stack.pop()

        if b == 0:
            raise VMError("Modulo by zero")

        self._charge(self._word_cost(a, b))
        self._push(a % b)

    def print_top(self):
        """Contract event: recorded in the receipt (and echoed in verbose mode)."""
        if self.stack:
            value = self.stack[-1]
            self.logs.append({"event": "print", "value": value})
            if self.verbose:
                print(value)

    def halt(self):
        self.running = False

    def nop(self):
        pass

    # ------------------------------------------------------------------ #
    # Control flow
    # ------------------------------------------------------------------ #

    def jmp(self):
        self.pc = self._read_target()

    def jz(self):
        self._require(1)
        condition = self.stack.pop()
        target = self._read_target()
        if not condition:
            self.pc = target

    def jnz(self):
        self._require(1)
        condition = self.stack.pop()
        target = self._read_target()
        if condition:
            self.pc = target

    # ------------------------------------------------------------------ #
    # Stack manipulation
    # ------------------------------------------------------------------ #

    def dup(self):
        if self.stack:
            self._push(self.stack[-1])

    def swap(self):
        if len(self.stack) >= 2:
            self.stack[-1], self.stack[-2] = self.stack[-2], self.stack[-1]

    def over(self):
        if len(self.stack) >= 2:
            self._push(self.stack[-2])

    def rot(self):
        if len(self.stack) >= 3:
            self.stack[-3], self.stack[-2], self.stack[-1] = (
                self.stack[-2],
                self.stack[-1],
                self.stack[-3],
            )

    # ------------------------------------------------------------------ #
    # Comparisons and logic
    # ------------------------------------------------------------------ #

    def AND(self):
        self._require(2)
        b = self.stack.pop()
        a = self.stack.pop()
        self._push(1 if (a and b) else 0)

    def OR(self):
        self._require(2)
        b = self.stack.pop()
        a = self.stack.pop()
        self._push(1 if (a or b) else 0)

    def XOR(self):
        self._require(2)
        b = self.stack.pop()
        a = self.stack.pop()
        self._push(1 if (bool(a) != bool(b)) else 0)

    @staticmethod
    def _is_empty(value) -> bool:
        """PENA treats 0, "" and empty collections as the same "empty" value.

        Contracts such as SANRC721 compare a missing mapping entry (0) against
        "" to detect "not minted", so equality must understand both as empty.
        """
        if isinstance(value, (list, dict, str)):
            return len(value) == 0
        return value in (0, None)

    @classmethod
    def _equal(cls, a, b) -> bool:
        if a == b:
            return True
        return cls._is_empty(a) and cls._is_empty(b)

    def eq(self):
        self._require(2)
        b = self.stack.pop()
        a = self.stack.pop()
        self._push(1 if self._equal(a, b) else 0)

    def neq(self):
        self._require(2)
        b = self.stack.pop()
        a = self.stack.pop()
        self._push(0 if self._equal(a, b) else 1)

    def lt(self):
        self._require(2)
        b = self.stack.pop()
        a = self.stack.pop()
        self._push(1 if a < b else 0)

    def lte(self):
        self._require(2)
        b = self.stack.pop()
        a = self.stack.pop()
        self._push(1 if a <= b else 0)

    def gt(self):
        self._require(2)
        b = self.stack.pop()
        a = self.stack.pop()
        self._push(1 if a > b else 0)

    def gte(self):
        self._require(2)
        b = self.stack.pop()
        a = self.stack.pop()
        self._push(1 if a >= b else 0)

    # ------------------------------------------------------------------ #
    # Variables (local scope first, then contract storage)
    # ------------------------------------------------------------------ #

    def set_var(self):
        self._require(2)
        value = self.stack.pop()
        key = self.stack.pop()

        for scope in reversed(self.scopes):
            if key in scope:
                scope[key] = value
                return

        self.storage.set_var(key, value)

    def get_var(self):
        self._require(1)
        key = self.stack.pop()

        for scope in reversed(self.scopes):
            if key in scope:
                self._push(scope[key])
                return

        self._push(self.storage.get_var(key))

    def delete_var(self):
        self._require(1)
        key = self.stack.pop()
        self.storage.delete_var(key)

    def has_var(self):
        self._require(1)
        key = self.stack.pop()
        exists = 1 if self.storage.has_var(key) else 0
        self._push(exists)

    # ------------------------------------------------------------------ #
    # Collections
    # ------------------------------------------------------------------ #

    def list_append(self):
        self._require(2)
        value = self.stack.pop()
        key = self.stack.pop()
        # Audit fix: growth is metered by the serialized size of the value.
        self._charge(payload_cost(value))

        if not self.storage.has_var(key):
            raise VMError(f"Unknown list: {key}")

        lst = self.storage.get_var(key)
        if not isinstance(lst, list):
            raise VMError(f"{key} is not a list")

        lst.append(value)
        self.storage.set_var(key, lst)

    def list_remove(self):
        self._require(2)
        value = self.stack.pop()
        key = self.stack.pop()
        self._charge(payload_cost(value))

        if not self.storage.has_var(key):
            raise VMError(f"Unknown list: {key}")

        lst = self.storage.get_var(key)
        if not isinstance(lst, list):
            raise VMError(f"{key} is not a list")

        if value in lst:
            lst.remove(value)
        self.storage.set_var(key, lst)

    def list_len(self):
        self._require(1)
        key = self.stack.pop()

        if not self.storage.has_var(key):
            self._push(0)
            return

        lst = self.storage.get_var(key)
        if not isinstance(lst, list):
            raise VMError(f"{key} is not a list")

        self._push(len(lst))

    def list_get(self):
        self._require(2)
        index = self.stack.pop()
        key = self.stack.pop()

        if not self.storage.has_var(key):
            raise VMError(f"Unknown list: {key}")

        lst = self.storage.get_var(key)
        if not isinstance(lst, list):
            raise VMError(f"{key} is not a list")

        if not isinstance(index, int) or not (0 <= index < len(lst)):
            raise VMError(f"{index} is an invalid index for {key}")

        self._push(lst[index])

    def dict_set(self):
        self._require(3)
        value = self.stack.pop()
        key_name = self.stack.pop()
        dict_name = self.stack.pop()
        self._charge(payload_cost(value) + payload_cost(key_name))

        if not self.storage.has_var(dict_name):
            raise VMError(f"Unknown subscript target: {dict_name}")

        container = self.storage.get_var(dict_name)

        if isinstance(container, dict):
            container[key_name] = value
        elif isinstance(container, list):
            if not isinstance(key_name, int) or not (0 <= key_name <= len(container)):
                raise VMError(f"{key_name} is an invalid index for {dict_name}")
            if key_name == len(container):
                container.append(value)
            else:
                container[key_name] = value
        else:
            raise VMError(f"{dict_name} is not subscriptable")

        self.storage.set_var(dict_name, container)

    def dict_get(self):
        self._require(2)
        key_name = self.stack.pop()
        dict_name = self.stack.pop()

        if not self.storage.has_var(dict_name):
            raise VMError(f"Unknown subscript target: {dict_name}")

        container = self.storage.get_var(dict_name)

        if isinstance(container, dict):
            # PENA contracts (e.g. token balances) rely on a 0 default for
            # missing keys, mirroring the language's untyped value model.
            self._push(container.get(key_name, 0))
            return

        if isinstance(container, list):
            if not isinstance(key_name, int) or not (0 <= key_name < len(container)):
                raise VMError(f"{key_name} is an invalid index for {dict_name}")
            self._push(container[key_name])
            return

        raise VMError(f"{dict_name} is not subscriptable")

    def dict_keys(self):
        self._require(1)
        dict_name = self.stack.pop()

        if not self.storage.has_var(dict_name):
            raise VMError(f"Unknown dict: {dict_name}")

        dictionary = self.storage.get_var(dict_name)
        if not isinstance(dictionary, dict):
            raise VMError(f"{dict_name} is not a dict")

        self._push(list(dictionary.keys()))

    # ------------------------------------------------------------------ #
    # Calls and functions
    # ------------------------------------------------------------------ #

    def call(self):
        self._require(1)
        address = self.stack.pop()
        if not isinstance(address, int):
            raise VMError(f"Invalid call address: {address!r}")
        if len(self.call_stack) >= self.max_call_depth:
            raise VMError(f"Call depth exceeded {self.max_call_depth}")
        self.call_stack.append({"pc": self.pc})
        self.pc = address

    def ret(self):
        if not self.call_stack:
            return
        frame = self.call_stack.pop()
        if self.scopes:
            self.scopes.pop()
        self.pc = frame["pc"]

    def end_function(self):
        self.ret()

    def define_function(self):
        self._require(2)
        param_count = self.stack.pop()
        if not isinstance(param_count, int) or param_count < 0:
            raise VMError(f"Invalid parameter count: {param_count!r}")

        self._require(param_count + 1)
        params = [self.stack.pop() for _ in range(param_count)][::-1]
        func_name = self.stack.pop()
        body_pc = self._read_target()

        self.storage.functions[func_name] = {
            "pc": body_pc,
            "params": params,
            "param_count": param_count,
        }

    def call_function(self):
        self._require(2)
        param_count = self.stack.pop()
        func_name = self.stack.pop()

        if not isinstance(param_count, int) or param_count < 0:
            raise VMError(f"Invalid parameter count: {param_count!r}")

        func_info = self.storage.functions.get(func_name)
        if func_info is None:
            raise VMError(f"Unknown function: {func_name}")

        if func_info["param_count"] != param_count:
            raise VMError(
                f"{func_name} expects {func_info['param_count']} parameter(s), "
                f"got {param_count}"
            )

        if len(self.call_stack) >= self.max_call_depth:
            raise VMError(f"Call depth exceeded {self.max_call_depth}")

        self._require(param_count)
        args = [self.stack.pop() for _ in range(param_count)][::-1]

        self.call_stack.append({"pc": self.pc})
        self.scopes.append(dict(zip(func_info["params"], args)))
        self.pc = func_info["pc"]

    # ------------------------------------------------------------------ #
    # Contracts
    # ------------------------------------------------------------------ #

    def deploy_contract(self, contract_id, bytecode, gas_limit=None):
        return self.contract_manager.deploy_contract(
            contract_id, bytecode, gas_limit=gas_limit
        )

    def call_contract_function(self, contract_id, function_name, params, gas_limit=None):
        return self.contract_manager.call_contract_function(
            contract_id, function_name, params, gas_limit=gas_limit
        )
