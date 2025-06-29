
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
| `PUSH`        | Push value to stack                |
| `POP`         | Remove top of stack                |
| `ADD`, `SUB`, `MUL`, `DIV`, `MOD` | Math ops     |
| `EQ`, `NEQ`, `LT`, `GT`, `GTE`, `LTE` | Comparison |
| `SET`, `GET`  | Variable storage access            |
| `PRINT`       | Output top stack item              |
| `DEF_FUNC`, `CALL_FUNC`, `RET` | Function handling |
| `FOR_LOOP`, `CONTINUE_LOOP`, `BREAK_LOOP` | Loops  |
| `IF`, `JMP`   | Conditional jumps                  |
| `LIST_APPEND`, `LIST_REMOVE`, `LIST_LEN`, `LIST_GET` | List ops |
| `DICT_SET`, `DICT_GET`, `DICT_KEYS` | Dict ops     |

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
- AST (abstract syntax tree) constructed
- Stack-based bytecode emitted via `PenaParser`
- Bytecode executed by `SANVirtualMachine`

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
