from SANVM.OpCode import OpCode

class Parser:
    @staticmethod
    def parse_instruction_list(instruction_list):
        bytecode = []

        for instruction in instruction_list:
            opcode_name = instruction[0]  # First opcode name

            if opcode_name in OpCode.__members__:  # if valid opcode
                opcode = OpCode[opcode_name].value  # Take opcode name
                bytecode.append(opcode)  # Add bytecode list

                # If it is PUSH, there must be operator
                if opcode == OpCode.PUSH.value:
                    bytecode.append(instruction[1])  # PUSH operand

            else:
                raise ValueError(f"Unvalid opcode: {opcode_name}")

        return bytecode
