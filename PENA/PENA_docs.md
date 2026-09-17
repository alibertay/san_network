
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

1. **PENA Code** → parsed by `PenaParser`
2. **Bytecode Output** → interpreted by `SANVirtualMachine`
3. **Storage and Execution** → handled via stack + storage model

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

## 📦 Supported Opcodes

| Opcode        | Description                        |
|---------------|------------------------------------|
| `PUSH`, `POP`, `DROP` | Stack handling              |
| `ADD`, `SUB`, `MUL`, `DIV`, `MOD` | Math ops     |
| `EQ`, `NEQ`, `LT`, `GT`, `GTE`, `LTE` | Comparison |
| `SET`, `GET`, `DELETE`, `HAS` | Variable storage access |
| `PRINT`       | Output top stack item              |
| `JMP`, `JZ`, `JNZ` | Jumps (`if`/`while`/`for` compile to these) |
| `DEF_FUNC`, `CALL_FUNC`, `END_FUNC`, `RET`, `CALL` | Function handling |
| `LIST_APPEND`, `LIST_REMOVE`, `LIST_LEN`, `LIST_GET` | List ops |
| `DICT_SET`, `DICT_GET`, `DICT_KEYS` | Dict/subscript ops |
| `DUP`, `SWAP`, `OVER`, `ROT` | Stack manipulation      |
| `AND`, `OR`, `XOR`, `NOP`, `HALT` | Logic / control      |

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

- Source is parsed → tokens extracted
- Symbols are emitted with symbolic labels; a second pass resolves them to
  real instruction indices, so jumps always land on valid positions
- Stack-based bytecode is emitted by `PenaParser`
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

---

PENA makes smart contract logic accessible, readable, and executable in a decentralized world.
