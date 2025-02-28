from OpCode import OpCode
from Storage import Storage
from SANVM.ContractManager import ContractManager

class SANVirtualMachine:
    def __init__(self, storage=None):
        self.stack = []
        self.call_stack = []
        self.loop_stack = []
        self.running = True
        self.pc = 0 # Bytecode queue
        self.bytecode = []

        self.storage = storage if storage else Storage()

        self.contract_manager = ContractManager(self.storage)

        self.instructions = {
            OpCode.PUSH.value: self.push,
            OpCode.POP.value: self.pop,
            OpCode.ADD.value: self.add,
            OpCode.SUB.value: self.sub,
            OpCode.MUL.value: self.mul,
            OpCode.DIV.value: self.div,
            OpCode.PRINT.value: self.print_top,
            OpCode.HALT.value: self.halt,
            OpCode.MOD.value: self.mod,
            OpCode.JMP.value: self.jmp,
            OpCode.IF.value: self.IF,
            OpCode.DUP.value: self.dup,
            OpCode.SWAP.value: self.swap,
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
            OpCode.OVER.value: self.over,
            OpCode.ROT.value: self.rot,
            OpCode.SET.value: self.set_var,
            OpCode.GET.value: self.get_var,
            OpCode.DELETE.value: self.delete_var,
            OpCode.HAS.value: self.has_var,
            OpCode.LIST_APPEND: self.list_append,
            OpCode.LIST_REMOVE: self.list_remove,
            OpCode.LIST_LEN: self.list_len,
            OpCode.LIST_GET: self.list_get,
            OpCode.DICT_SET: self.dict_set,
            OpCode.DICT_GET: self.dict_get,
            OpCode.DICT_KEYS: self.dict_keys,
            OpCode.FOR_LOOP: self.for_loop,
            OpCode.BREAK_LOOP: self.break_loop,
            OpCode.CONTINUE_LOOP: self.continue_loop,
            OpCode.DEF_FUNC: self.define_function,
            OpCode.CALL_FUNC: self.call_function
        }

    def run(self, bytecode):
        self.bytecode = bytecode
        self.pc = 0

        while self.running and self.pc < len(self.bytecode):
            opcode = self.bytecode[self.pc]
            self.pc += 1

            # if opcode defined, call function
            if opcode in self.instructions:
                self.instructions[opcode]()
            else:
                raise ValueError(f"Unknown opcode: {opcode}")

    def push(self):
        value = self.bytecode[self.pc]
        self.pc += 1
        self.stack.append(value)

    def pop(self):
        if self.stack:
            self.stack.pop()

    def add(self):
        if len(self.stack) >= 2:
            b = self.stack.pop()
            a = self.stack.pop()
            self.stack.append(a + b)

    def sub(self):
        if len(self.stack) >= 2:
            b = self.stack.pop()
            a = self.stack.pop()
            self.stack.append(a - b)

    def mul(self):
        if len(self.stack) >= 2:
            b = self.stack.pop()
            a = self.stack.pop()
            self.stack.append(a * b)

    def div(self):
        if len(self.stack) >= 2:
            b = self.stack.pop()
            a = self.stack.pop()

            if b == 0:
                raise ZeroDivisionError("Zero division error")

            self.stack.append(a // b)

    def print_top(self):
        if self.stack:
            print(self.stack[-1])

    def mod(self):
        if len(self.stack) >= 2:
            b = self.stack.pop()
            a = self.stack.pop()

            if b == 0:
                raise ZeroDivisionError("Zero division error")

            self.stack.append(a % b)

    def jmp(self):
        if self.pc < len(self.bytecode):
            target = self.bytecode[self.pc]
            self.pc = target

    def IF(self):
        if self.stack:
            condition = self.stack.pop()
            expected_value = self.bytecode[self.pc]
            self.pc += 1

            if condition != expected_value:
                self.pc += 1

    def dup(self):
        if self.stack:
            self.stack.append(self.stack[:-1])

    def over(self):
        if len(self.stack) >= 2:
            self.stack.append(self.stack[-2])

    def rot(self):
        if len(self.stack) >= 3:
            self.stack[-3], self.stack[-2], self.stack[-1] = (
                self.stack[-2], self.stack[-1], self.stack[-3]
            )

    def swap(self):
        if len(self.stack) >= 2:
            self.stack[-1], self.stack[-2] = self.stack[-2], self.stack[-1]

    def set_var(self):
        if len(self.stack) >= 2:
            value = self.stack.pop()
            key = self.stack.pop()
            self.storage.set_var(key, value)

    def get_var(self):
        if self.stack:
            key = self.stack.pop()
            value = self.storage.get_var(key)
            self.stack.append(value)

    def delete_var(self):
        if self.stack:
            key = self.stack.pop()
            self.storage.delete(key)

    def has_var(self):
        if self.stack:
            key = self.stack.pop()
            exists = 1 if self.storage.has_var(key) else 0
            self.stack.append(exists)

    def list_append(self):
        if len(self.stack) >= 2:
            value = self.stack.pop()
            key = self.stack.pop()

            if not self.storage.has_var(key):
                raise KeyError(f"Unknown list: {key}")

            lst = self.storage.get_var(key)
            if not isinstance(lst, list):
                raise TypeError(f"{key} is not a list")

            lst.append(value)
            self.storage.set_var(key, lst)

    def list_remove(self):
        if len(self.stack) >= 2:
            value = self.stack.pop()
            key = self.stack.pop()

            if not self.storage.has_var(key):
                raise KeyError(f"Unkown list: {key}")

            lst = self.storage.get_var(key)
            if not isinstance(lst, list):
                raise TypeError(f"{key} is not a list")

            if value in lst:
                lst.remove(value)
            self.storage.set_var(key, lst)

    def list_len(self):
        if self.stack:
            key = self.stack.pop()

            if not self.storage.has_var(key):
                self.stack.append(0)
                return

            lst = self.storage.get_var(key)
            if not isinstance(lst, list):
                raise TypeError(f"{key} is not a list")

            self.stack.append(len(lst))

    def list_get(self):
        if len(self.stack) >= 2:
            index = self.stack.pop()
            key = self.stack.pop()

            if not self.storage.has_var(key):
                raise KeyError(f"Unkown list: {key}")

            lst = self.storage.get_var(key)
            if not isinstance(lst, list):
                raise TypeError(f"{key} is not a list")

            if not(0 <= index < len(lst)):
                raise IndexError(f"{index} is an invalid index for {key}")

            self.stack.append(lst[index])

    def dict_set(self):
        if len(self.stack) >= 3:
            value = self.stack.pop()
            key_name = self.stack.pop()
            dict_name = self.stack.pop()

            if not self.storage.has_var(dict_name):
                raise KeyError(f"Unkown dict: {dict_name}")

            dictionary = self.storage.get_var(dict_name)
            if not isinstance(dictionary, dict):
                raise TypeError(f"{dict_name} is not a dict")

            dictionary[key_name] = value
            self.storage.set_var(dict_name, dictionary)

    def dict_get(self):
        if len(self.stack) >= 2:
            key_name = self.stack.pop()
            dict_name = self.stack.pop()

            if not self.storage.has_var(dict_name):
                raise KeyError(f"Unkown dict: {dict_name}")

            dictionary = self.storage.get_var(dict_name)
            if not isinstance(dictionary, dict):
                raise TypeError(f"{dict_name} is not a dict")

            if key_name not in dictionary:
                raise KeyError(f"{key_name} not found in {dict_name}")

            self.stack.append(dictionary[key_name])

    def dict_keys(self):
        if self.stack:
            dict_name = self.stack.pop()

            if not self.storage.has_var(dict_name):
                raise KeyError(f"Unkown dict: {dict_name}")

            dictionary = self.storage.get_var(dict_name)
            if not isinstance(dictionary, dict):
                raise TypeError(f"{dict_name} is not a dict")

            self.stack.append(list(dictionary.keys()))

    def AND(self):
        if len(self.stack) >= 2:
            b = self.stack.pop()
            a = self.stack.pop()
            self.stack.append(1 if (a != 0 and b != 0) else 0)

    def OR(self):
        if len(self.stack) >= 2:
            b = self.stack.pop()
            a = self.stack.pop()
            self.stack.append(1 if (a != 0 or b != 0) else 0)

    def XOR(self):
        if len(self.stack) >= 2:
            b = self.stack.pop()
            a = self.stack.pop()
            self.stack.append(1 if (bool(a) != bool(b)) else 0)

    def eq(self):
        if len(self.stack) >= 2:
            b = self.stack.pop()
            a = self.stack.pop()
            self.stack.append(1 if a == b else 0)

    def neq(self):
        if len(self.stack) >= 2:
            b = self.stack.pop()
            a = self.stack.pop()
            self.stack.append(1 if a != b else 0)

    def lt(self):
        if len(self.stack) >= 2:
            b = self.stack.pop()
            a = self.stack.pop()
            self.stack.append(1 if a < b else 0)

    def lte(self):
        if len(self.stack) >= 2:
            b = self.stack.pop()
            a = self.stack.pop()
            self.stack.append(1 if a <= b else 0)

    def gt(self):
        if len(self.stack) >= 2:
            b = self.stack.pop()
            a = self.stack.pop()
            self.stack.append(1 if a > b else 0)

    def gte(self):
        if len(self.stack) >= 2:
            b = self.stack.pop()
            a = self.stack.pop()
            self.stack.append(1 if a >= b else 0)

    def call(self):
        if self.stack:
            address = self.stack.pop()
            self.call_stack.append(self.pc)
            self.pc = address

    def ret(self):
        if self.call_stack:
            last_call = self.call_stack.pop()
            self.pc = last_call["pc"]

    def nop(self):
        pass

    def for_loop(self):
        if len(self.stack) >= 2:
            iterations = self.stack.pop()
            counter_var = self.stack.pop()

            if not isinstance(iterations, int) or iterations <= 0:
                raise ValueError(f"Unvalide usage: {iterations}")

            start_pc = self.pc
            self.loop_stack.append((counter_var, iterations, start_pc))
            self.storage.set_var(counter_var, 0)

    def break_loop(self):
        if self.loop_stack:
            _, _, loop_end = self.loop_stack.pop()
            self.pc = loop_end

    def continue_loop(self):
        if self.loop_stack:
            counter_var, iterations, loop_start = self.loop_stack[-1]
            count = self.storage.get_var(counter_var)

            if count + 1 >= iterations:
                self.loop_stack.pop()
                return

            self.storage.set_var(counter_var, count + 1)
            self.pc = loop_start

    def define_function(self):
        if len(self.stack) >= 2:
            param_count = self.stack.pop()
            func_name = self.stack.pop()

            self.storage.functions[func_name] = {
                "pc": self.pc,
                "param_count": param_count
            }

        while self.pc < len(self.bytecode) and self.bytecode[self.pc] != OpCode.RET.value:
            self.pc += 1

    def call_function(self):
        if len(self.stack) >= 2:
            param_count = self.stack.pop()
            func_name = self.stack.pop()

            if func_name not in self.storage.functions:
                raise KeyError(f"Unkown function: {func_name}")

            func_info = self.storage.functions[func_name]
            if func_info["param_count"] != param_count:
                raise ValueError(f"{func_name} need {func_info['param_count']} param")

            self.call_stack.append({
                "pc": self.pc,
                "params": [self.stack.pop() for _ in range(param_count)]
            })

            self.pc = func_info["pc"]

    def halt(self):
        self.running = False

    def deploy_contract(self, contract_id, bytecode):
        return self.contract_manager.deploy_contract(contract_id, bytecode)

    def call_contract_function(self, contract_id, function_name, params):
        return self.contract_manager.call_contract_function(contract_id, function_name, params)