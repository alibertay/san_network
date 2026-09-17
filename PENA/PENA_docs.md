
# 📘 PENA Programming Language Documentation

PENA (Program Execution & Native Assembly) is a human-friendly, high-level programming language that compiles into stack-based bytecode designed to run on the SAN Virtual Machine (SANVM). It simplifies the process of writing and executing smart contracts in decentralized environments.

---

## 🧠 Language Philosophy
- Stack-based, minimal, and efficient
- Python-inspired human-readable syntax
- Translates directly into deterministic bytecode
- Designed for clarity, auditability, and on-chain execution

---

## 🏗️ Execution Architecture

1. **PENA Code** → lowered by `PenaParser` to **PENA Assembly (PASM)**
2. **Assembly** → assembled (macros expanded, labels resolved) by `SANVM.asm`
3. **Bytecode** → interpreted by `SANVirtualMachine`
4. **Storage and Execution** → handled via stack + storage model

High-level PENA and hand-written assembly share the same encoder, so both
produce identical bytecode for identical programs, and every opcode is
reachable from assembly.

---

## ✨ Syntax Guide

### ➕ Variable Assignment
```pena
x = 10
y = (x + 5) * 2
```

### 📤 Print
```pena
print("Hello World")
```

### 🔁 For Loops
```pena
for i, 0 -> 5 {
  print(i)
}
```

### 🔄 While Loops
```pena
x = 0
while (x < 10) {
  print(x)
  x = x + 1
}
```

### 🔂 Break & Continue
```pena
for i, 0 -> 10 {
  if (i == 5) {
    break
  }
  if (i % 2 == 0) {
    continue
  }
  print(i)
}
```

### 🔀 If / Else If / Else
```pena
if (x > 10) {
  print("Big")
}
else if (x < 5) {
  print("Small")
}
else {
  print("Medium")
}
```

### 🧩 Functions
```pena
function add(a, b) {
  return a + b
}
```

### 📞 Function Calls
```pena
woof add(10, 20)
print(add(1, 2))    // calls are also allowed inside expressions
```

### ➗ Operators

```pena
x = (3 + 5) * 2 - 4 / 2 % 3
if (a == b && c != d || !flag) {
  print("picked")
}
```

Supported operators: `+ - * / %` (`/` is integer division), `== != < <= > >=`,
`&& || !`, and unary minus.

### 🔎 Subscripts

```pena
balances := {}
balances[owner] = 100
print(balances[owner])     // missing keys read as 0

mylist := [1, 2, 3]
print(mylist[1])
```

---

## 📚 Data Structures

### 📋 Lists
```pena
mylist := [1, 2, 3]
```

### 🧾 Dictionaries
```pena
mydict := {}
```

---

## ⚙️ PENA Assembly (PASM)

PENA Assembly is the textual instruction layer of the SANVM. High-level PENA
is compiled down to assembly and assembly is encoded into the flat bytecode
executed by the VM, so every contract has two equivalent forms:

1. **High-level PENA** – the human-friendly syntax documented above
2. **PENA Assembly (PASM)** – one instruction per line, assembled by
   `SANVM.asm.assemble`

### Assembly Syntax

```asm
; comments start with ';' or '//'
PUSH 5
PUSH 10
ADD
PRINT
HALT
```

- Mnemonics are case-insensitive; one instruction per line.
- Literals: integers (`10`, `0xFF`, `0b101`), floats, strings (`"hi"`),
  lists (`[1, 2, 3]`) and dicts (`{"k": 1}`).
- Bare identifiers are symbols (strings): `PUSH greet` == `PUSH "greet"`.
- `true` / `false` are the integers `1` / `0`.
- Labels are defined as `name:` (an instruction may follow on the same line)
  and referenced by `JMP`, `JZ`, `JNZ` and `DEF_FUNC`. Forward references are
  resolved by a two-pass assembly.

```asm
; sum 1..5 and print the total
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
```

### Variable Macros

These mnemonics accept an optional variable name and expand to `PUSH "name"`
plus the opcode; without an operand they emit the plain opcode and take their
operands from the stack:

| Macro | Expansion | Meaning |
|-------|-----------|---------|
| `GET x` | `PUSH "x", GET` | read variable `x` |
| `SET x` | `PUSH "x", SWAP, SET` | store the pending value in `x` |
| `HAS x` | `PUSH "x", HAS` | 1 if `x` exists |
| `DELETE x` | `PUSH "x", DELETE` | remove `x` |
| `LIST_LEN x` | `PUSH "x", LIST_LEN` | push the list length |
| `LIST_APPEND x` | `PUSH "x", SWAP, LIST_APPEND` | append the pending value |
| `LIST_REMOVE x` | `PUSH "x", SWAP, LIST_REMOVE` | remove the pending value |
| `LIST_GET x` | `PUSH "x", SWAP, LIST_GET` | read the pending index |
| `DICT_SET x` | `PUSH "x", ROT, ROT, DICT_SET` | write the pending key/value |
| `DICT_GET x` | `PUSH "x", SWAP, DICT_GET` | read the pending key |
| `DICT_KEYS x` | `PUSH "x", DICT_KEYS` | push the list of keys |

### Function Blocks

`FUNC name(params) { ... }` defines a contract function from within assembly;
the assembler emits the same `DEF_FUNC` / `JMP` / `END_FUNC` sequence that the
high-level compiler emits:

```asm
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
```

### Inline Assembly

Any high-level program may embed assembly blocks. They are assembled in place
and their labels are namespaced per block, so label names can be reused:

```pena
counter = 0

function bump() {
  asm {
    GET counter
    PUSH 1
    ADD
    SET counter
  }
}
```

### Compiling and Selecting the Language

- `SANVM.asm.assemble(source)` compiles assembly; `SANVM.asm.disassemble(bytecode)`
  renders bytecode back to assembly.
- `SANVM.pena_parser.compile_pena(source, language=None)` auto-detects:
  sources whose first meaningful line is an upper-case mnemonic, macro, `FUNC`
  or label are assembled; everything else is compiled as high-level PENA.
  Pass `language="asm"` or `language="pena"` to force a compiler.
- Deployments accept both forms through `pena_code`; an optional
  `"language": "asm"` / `"language": "pena"` field overrides auto-detection.
- Lower-case assembly mnemonics are accepted when `language="asm"` is
  explicit (auto-detection requires upper case to stay unambiguous with
  high-level syntax such as `print(...)`).

### Full Opcode / Mnemonic Reference

| Mnemonic | Operand | Description |
|----------|---------|-------------|
| `PUSH` | literal | push a literal (values are charged by size) |
| `POP`, `DROP` | – | discard the top stack item |
| `ADD`, `SUB`, `MUL`, `DIV`, `MOD` | – | integer math (`/` floor, `%` modulo) |
| `EQ`, `NEQ`, `LT`, `LTE`, `GT`, `GTE` | – | comparisons (push `1` / `0`) |
| `AND`, `OR`, `XOR` | – | boolean logic (push `1` / `0`) |
| `PRINT` | – | emit the top stack item as a contract event |
| `JMP` | label/address | unconditional jump |
| `JZ` | label/address | jump when the popped value is falsy |
| `JNZ` | label/address | jump when the popped value is truthy |
| `DUP`, `SWAP`, `OVER`, `ROT` | – | stack manipulation |
| `SET`, `GET`, `DELETE`, `HAS` | – (`x` via macro) | variable storage access |
| `LIST_APPEND`, `LIST_REMOVE`, `LIST_LEN`, `LIST_GET` | – (`x` via macro) | list operations |
| `DICT_SET`, `DICT_GET`, `DICT_KEYS` | – (`x` via macro) | dict / subscript operations |
| `CALL` | – | jump to the address on the stack (subroutine) |
| `RET` | – | return from a call |
| `DEF_FUNC` | label/address | register a function (`name`, params, count on stack) |
| `CALL_FUNC` | – | call a registered function (`name`, argument count on stack) |
| `END_FUNC` | – | end of a function body (implicit return) |
| `NOP`, `HALT` | – | no-op / stop execution |

---

## 🧪 Smart Contract Example (Deployable)

```pena
function greet(name) {
  print("Hello " + name)
}

woof greet("Alice")
```

### JSON Deployment Example:
```json
{
  "contract_code": {
    "command": "deploy",
    "contract_id": "hello_contract",
    "pena_code": "function greet(name) { print(\"Hello \" + name) }"
  }
}
```

### JSON Execution Example:
```json
{
  "contract_code": {
    "command": "run",
    "contract_id": "hello_contract",
    "function_name": "greet",
    "params": ["Alice"]
  }
}
```

---

## 🔁 Parsing & Execution Flow

- Source is preprocessed; `asm { ... }` blocks are extracted (brace and
  string aware) and parsed by the assembler with a per-block label namespace
- High-level statements and expressions are lowered to PENA Assembly
  instructions with symbolic labels; inline blocks are merged in place
- The assembler expands macros / `FUNC` blocks in one pass and resolves every
  label to a real instruction index in a second pass, so jumps always land on
  valid positions
- Stack-based bytecode is produced by the shared encoder (`SANVM.asm.encode`)
- Bytecode is executed by `SANVirtualMachine`, which enforces step (gas),
  stack and call-depth limits

## 📐 Semantics Notes

- Function parameters are local to the call; every other name reads/writes
  contract storage.
- A missing dictionary key reads as `0`, and `0`, `""`, `[]`, `{}` compare
  equal — contracts use patterns like `if (owner_of[id] == "")` to detect
  missing entries.
- All numbers are integers; `/` is integer division, `%` is modulo.
- Contract calls are atomic: a failing call leaves the previous storage
  snapshot untouched.

---

## 🌍 Use Cases

- On-chain contract scripting
- Inter-node communication logic
- Teaching stack machine and compiler design
- Lightweight deterministic automation

---

## 🚧 Future Roadmap

- Event emission (`emit`)
- Import system and code modularity
- Native JSON-like structure support
- VM-level debugging / logging tools
- Assembly-level optimizer / peephole passes

---

PENA makes smart contract logic accessible, readable, and executable in a decentralized world.
