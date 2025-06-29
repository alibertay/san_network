import re
from typing import List, Union
from OpCode import OpCode

class PenaParser:
    def __init__(self):
        self.bytecode: List[Union[int, str]] = []
        self.label_counter = 0

    def parse(self, source: str) -> List[Union[int, str]]:
        self.bytecode = []
        self.label_counter = 0
        lines = self._preprocess(source)
        i = 0
        while i < len(lines):
            line = lines[i]
            if line.startswith("function"):
                i = self._parse_function(lines, i)
            elif line.startswith("for"):
                i = self._parse_for(lines, i)
            elif line.startswith("while"):
                i = self._parse_while(lines, i)
            elif line.startswith("if"):
                i = self._parse_if(lines, i)
            elif line.startswith("woof "):
                self._parse_function_call(line)
                i += 1
            elif line.startswith("print("):
                self._parse_print(line)
                i += 1
            elif line.startswith("return"):
                self._parse_return(line)
                i += 1
            elif line.strip() == "break":
                self.bytecode.append(OpCode.BREAK_LOOP.value)
                i += 1
            elif line.strip() == "continue":
                self.bytecode.append(OpCode.CONTINUE_LOOP.value)
                i += 1
            elif ":=" in line:
                self._parse_struct_literal(line)
                i += 1
            elif "=" in line:
                self._parse_assignment(line)
                i += 1
            else:
                i += 1
        return self.bytecode

    def _preprocess(self, source: str) -> List[str]:
        lines = source.splitlines()
        return [line.strip() for line in lines if line.strip() and not line.strip().startswith("//")]

    def _parse_assignment(self, line: str):
        var, expr = map(str.strip, line.split("=", 1))
        tokens = self._tokenize_expression(expr)
        self._compile_expression(tokens)
        self.bytecode.extend([OpCode.PUSH.value, var, OpCode.SET.value])

    def _parse_struct_literal(self, line: str):
        # Example: mylist := [1, 2, 3]
        var, value = map(str.strip, line.split(":=", 1))
        if value.startswith("["):
            items = [v.strip() for v in value.strip("[] ").split(",") if v.strip()]
            self.bytecode.extend([OpCode.PUSH.value, var, OpCode.PUSH.value, [] , OpCode.SET.value])
            for item in items:
                self.bytecode.extend([OpCode.PUSH.value, var])
                self.bytecode.extend([OpCode.PUSH.value, int(item) if item.isnumeric() else item])
                self.bytecode.append(OpCode.LIST_APPEND.value)
        elif value.startswith("{"):
            self.bytecode.extend([OpCode.PUSH.value, var, OpCode.PUSH.value, {}, OpCode.SET.value])

    def _parse_print(self, line: str):
        expr = re.match(r"print\((.*)\)", line).group(1)
        tokens = self._tokenize_expression(expr)
        self._compile_expression(tokens)
        self.bytecode.append(OpCode.PRINT.value)

    def _parse_return(self, line: str):
        expr = line[len("return"):].strip()
        tokens = self._tokenize_expression(expr)
        self._compile_expression(tokens)
        self.bytecode.append(OpCode.RET.value)

    def _parse_function(self, lines: List[str], start_index: int) -> int:
        header = lines[start_index]
        name, params = re.match(r"function (\w+)\((.*?)\)", header).groups()
        param_list = [p.strip() for p in params.split(",") if p.strip()]
        self.bytecode.extend([OpCode.PUSH.value, name, OpCode.PUSH.value, len(param_list), OpCode.DEF_FUNC.value])
        i = start_index + 1
        while i < len(lines) and not lines[i].startswith("}"):
            i = self._parse_generic_line(lines, i)
        self.bytecode.append(OpCode.RET.value)
        return i + 1

    def _parse_function_call(self, line: str):
        name, args = re.match(r"(\w+)\((.*)\)", line[len("woof "):].strip()).groups()
        arg_list = [a.strip().strip('"') if '"' in a else a.strip() for a in args.split(",") if a.strip()]
        for arg in reversed(arg_list):
            self.bytecode.extend([OpCode.PUSH.value, arg])
        self.bytecode.extend([OpCode.PUSH.value, len(arg_list), OpCode.PUSH.value, name, OpCode.CALL_FUNC.value])

    def _parse_for(self, lines: List[str], start_index: int) -> int:
        header = lines[start_index]
        var, start, end = re.match(r"for (\w+), (\d+) -> (\d+)", header).groups()
        self.bytecode.extend([OpCode.PUSH.value, var, OpCode.PUSH.value, int(end), OpCode.FOR_LOOP.value])
        i = start_index + 1
        while i < len(lines) and not lines[i].startswith("}"):
            i = self._parse_generic_line(lines, i)
        self.bytecode.append(OpCode.CONTINUE_LOOP.value)
        return i + 1

    def _parse_while(self, lines: List[str], start_index: int) -> int:
        label_start = self._new_label()
        label_end = self._new_label()
        condition = re.search(r"\((.*?)\)", lines[start_index]).group(1)
        self.bytecode.append(label_start)
        tokens = self._tokenize_expression(condition)
        self._compile_expression(tokens)
        self.bytecode.extend([OpCode.IF.value, 1, OpCode.JMP.value, label_end])
        i = start_index + 1
        while i < len(lines) and not lines[i].startswith("}"):
            i = self._parse_generic_line(lines, i)
        self.bytecode.extend([OpCode.JMP.value, label_start])
        self.bytecode.append(label_end)
        return i + 1

    def _parse_if(self, lines: List[str], start_index: int) -> int:
        end_label = self._new_label()
        jump_labels = []
        i = start_index
        while i < len(lines):
            line = lines[i]
            if line.startswith("if") or line.startswith("else if"):
                condition = re.search(r"\((.*?)\)", line).group(1)
                tokens = self._tokenize_expression(condition)
                self._compile_expression(tokens)
                label = self._new_label()
                jump_labels.append(label)
                self.bytecode.extend([OpCode.IF.value, 1, OpCode.JMP.value, label])
                i += 1
                while i < len(lines) and not lines[i].startswith("}"):
                    i = self._parse_generic_line(lines, i)
                self.bytecode.extend([OpCode.JMP.value, end_label])
                self.bytecode.append(label)
                i += 1
            elif line.startswith("else"):
                i += 1
                while i < len(lines) and not lines[i].startswith("}"):
                    i = self._parse_generic_line(lines, i)
                i += 1
                break
            else:
                break
        self.bytecode.append(end_label)
        return i

    def _parse_generic_line(self, lines: List[str], i: int) -> int:
        line = lines[i]
        if line.startswith("print("):
            self._parse_print(line)
        elif line.startswith("return"):
            self._parse_return(line)
        elif line.startswith("woof "):
            self._parse_function_call(line)
        elif line.strip() == "break":
            self.bytecode.append(OpCode.BREAK_LOOP.value)
        elif line.strip() == "continue":
            self.bytecode.append(OpCode.CONTINUE_LOOP.value)
        elif ":=" in line:
            self._parse_struct_literal(line)
        elif "=" in line:
            self._parse_assignment(line)
        return i + 1

    def _new_label(self):
        self.label_counter += 1
        return f"LABEL_{self.label_counter}"

    def _tokenize_expression(self, expr: str) -> List[str]:
        return re.findall(r'\w+|[()+\-*/]', expr)

    def _compile_expression(self, tokens: List[str]):
        output = []
        ops = []
        precedence = {'+': 1, '-': 1, '*': 2, '/': 2}
        for token in tokens:
            if token.isnumeric() or token.isidentifier():
                output.append(token)
            elif token in precedence:
                while ops and precedence.get(ops[-1], 0) >= precedence[token]:
                    output.append(ops.pop())
                ops.append(token)
            elif token == '(':
                ops.append(token)
            elif token == ')':
                while ops and ops[-1] != '(':
                    output.append(ops.pop())
                ops.pop()
        output.extend(reversed(ops))
        for token in output:
            if token.isnumeric():
                self.bytecode.extend([OpCode.PUSH.value, int(token)])
            elif token.isidentifier():
                self.bytecode.extend([OpCode.PUSH.value, token, OpCode.GET.value])
            elif token == '+':
                self.bytecode.append(OpCode.ADD.value)
            elif token == '-':
                self.bytecode.append(OpCode.SUB.value)
            elif token == '*':
                self.bytecode.append(OpCode.MUL.value)
            elif token == '/':
                self.bytecode.append(OpCode.DIV.value)
