"""Generate golden fixtures for the Go migration parity tests.

Run from the repository root:

    python tools/parity_fixtures.py

The fixtures are written to ``internal/parity/testdata/foundation.json`` and
are derived exclusively from the Python implementation, which is the
reference for the Go port.
"""

from __future__ import annotations

import json
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

from blockchain import crypto  # noqa: E402
from blockchain.Block import Block  # noqa: E402
from blockchain.Blockchain import GENESIS_MESSAGE, Blockchain  # noqa: E402
from blockchain.Transaction import Transaction  # noqa: E402
from blockchain.address import (  # noqa: E402
    address_from_public_key,
    is_valid_address,
    normalize_address,
)
from blockchain.economics import (  # noqa: E402
    fee_rate_for_transaction_count,
    next_base_fee,
    san_to_units,
    units_to_san,
)
from blockchain.merkle import (  # noqa: E402
    leaf_hash,
    merkle_proof,
    merkle_root,
    state_entries,
    state_entry_proof,
    state_root,
    verify_merkle_proof,
)
from blockchain.persistence import ChainStore  # noqa: E402
from blockchain.storage import MemoryStore  # noqa: E402
from utils import canonical  # noqa: E402

CHAIN_ID = "san-devnet-1"
SENDER_PUBLIC, SENDER_PRIVATE = crypto.generate_keypair()
RECEIVER = "0x" + "bb" * 20

VALUES = [
    {"b": 1, "a": [2, 3]},
    {"unicode": "Türkçe ğüşiöç 😀", "accent": "é", "cyrillic": "привет"},
    {"escapes": 'line\n\ttab"quote\\back\r\b\f', "control": "\x00\x1f\x7f"},
    {"floats": [1.0, -0.0, 0.1, 1e-8, 1e16, 1e-7, 3.141592653589793, 1e300, 5e-324]},
    {"big": 10 ** 30, "neg": -(10 ** 25)},
    {"bools": [True, False, None]},
    {"nested": {"a": {"b": {"c": [1, {"d": 2}]}}}},
    {"empty": [{}, [], "", 0]},
    {"keys": {"10": 1, "2": 2, "ä": 3, "A": 4, "a": 5}},
    {1: "int-key", 10: "ten"},
    {"emoji": "🚀🌟", "turkish": "şğüöçıİ"},
    {"deep": [[[[[{"x": [1, 2, 3]}]]]]]},
]


def canonical_fixtures():
    fixtures = []
    for value in VALUES:
        fixtures.append(
            {
                "input": json.dumps(value, ensure_ascii=False),
                "expected": canonical.dumps(value),
            }
        )
    return fixtures


FLOAT_VALUES = [
    0.0,
    -0.0,
    1.0,
    0.1,
    1e-4,
    1e-5,
    1e15,
    1e16,
    1e20,
    100.0,
    123456789.123,
    2.2250738585072014e-308,
    5e-324,
    1.7976931348623157e308,
    -123.456,
    0.000123456,
]


def float_fixtures():
    return [{"value": value, "expected": repr(value)} for value in FLOAT_VALUES]


def address_fixtures():
    key_hex = crypto.generate_keypair()[0].hex()
    random_hex = os.urandom(1312).hex()
    return {
        "valid": [
            {"public_key": bytes(range(256)).hex(), "address": address_from_public_key(bytes(range(256)))},
            {"public_key": key_hex, "address": address_from_public_key(key_hex)},
            {"public_key": "0x" + key_hex, "address": address_from_public_key("0x" + key_hex)},
            {"public_key": random_hex, "address": address_from_public_key(random_hex)},
        ],
        "addresses": {
            "valid": [
                "0x" + "ab" * 20,
                "0x" + "00" * 20,
                "0x" + "FF" * 20,
            ],
            "invalid": [
                "0x1234",
                "ab" * 20,
                "0x" + "zz" * 20,
                "",
                123,
                None,
                "0x" + "ab" * 19,
                "0x" + "ab" * 21,
            ],
        },
    }


def merkle_fixtures():
    leaves = [
        "alpha",
        7,
        {"tx": "abc", "nonce": 1},
        [1, 2, 3],
        "omega",
    ]
    proofs = []
    for index in range(len(leaves)):
        proof = merkle_proof(leaves, index)
        proofs.append(
            {
                "index": index,
                "proof": proof,
                "valid": verify_merkle_proof(merkle_root(leaves), leaves[index], proof, index),
            }
        )
    return {
        "leaves": leaves,
        "root": merkle_root(leaves),
        "empty_root": merkle_root([]),
        "leaf_hashes": [
            {"leaf": leaf, "hash": leaf_hash(leaf)} for leaf in leaves
        ],
        "proofs": proofs,
    }


STATE = {
    "balances": {"0x" + "aa" * 20: 150, "0x" + "bb" * 20: 0},
    "nonces": {"0x" + "aa" * 20: 3, "0x" + "cc" * 20: 1},
    "validators": {
        "0x" + "aa" * 20: {
            "public_key": "deadbeef",
            "stake": 10 ** 12,
            "joined_height": 5,
            "release_height": None,
        }
    },
    "total_slashed": 7,
    "storage": {
        "contracts": {
            "token": {"bytecode": [1, "balance", {}, 29], "storage": {"data": {"supply": 1000}}},
            "empty": None,
        },
        "data": {"global": 1},
        "functions": {"f": {"pc": 3, "params": [], "param_count": 0}},
    },
    "parameters": {"base_fee": 2, "slash_bps": 5000},
    "base_fee": 3,
    "total_burned": 11,
}


def state_fixtures():
    entries = state_entries(
        STATE["balances"],
        STATE["nonces"],
        STATE["validators"],
        STATE["total_slashed"],
        STATE["storage"],
        STATE["parameters"],
        STATE["base_fee"],
        STATE["total_burned"],
    )
    return {
        "input": STATE,
        "entries": entries,
        "root": state_root(
            STATE["balances"],
            STATE["nonces"],
            STATE["validators"],
            STATE["total_slashed"],
            STATE["storage"],
            STATE["parameters"],
            STATE["base_fee"],
            STATE["total_burned"],
        ),
        "account_proof": state_entry_proof(
            STATE["balances"],
            STATE["nonces"],
            STATE["validators"],
            STATE["total_slashed"],
            STATE["storage"],
            "acct:" + "0x" + "aa" * 20,
            STATE["parameters"],
            STATE["base_fee"],
            STATE["total_burned"],
        ),
    }


def economics_fixtures():
    valid_amounts = ["1", "0.01", "0", "1.5", "1e-8", "-3", "1.00000000", 123456789]
    invalid_amounts = ["0.000000001", "abc", "", "NaN", "Infinity"]
    return {
        "san_to_units": [
            {"amount": amount, "units": san_to_units(amount)} for amount in valid_amounts
        ],
        "invalid_amounts": invalid_amounts,
        "units_to_san": [
            {"units": units, "text": str(units_to_san(units))}
            for units in [0, 1, 10, 100, 123456789, 100000000, 150000000,
                          100000001, 12345678900, 10 ** 18, 123000000,
                          999999999999, -5, -100000000]
        ],
        "fee_rate": [
            {"count": count, "rate": fee_rate_for_transaction_count(count)}
            for count in [0, 1, 99, 100, 999, 1000, 1001, 10 ** 9, -5]
        ],
        "next_base_fee": [
            {"current": current, "used": used, "limit": limit,
             "fee": next_base_fee(current, used, limit)}
            for current, used, limit in [
                (1, 0, 30_000_000),
                (1, 15_000_000, 30_000_000),
                (1, 30_000_000, 30_000_000),
                (10, 0, 30_000_000),
                (10, 30_000_000, 30_000_000),
                (0, 1, 30_000_000),
                (100, 29_999_999, 30_000_000),
                (1, 30_000_000, 0),
            ]
        ],
    }


def identity_fixtures():
    public_key, private_key = crypto.generate_keypair()
    message = b"san parity message"
    signature = crypto.sign(message, private_key)
    return {
        "private_key_hex": private_key.hex(),
        "public_key_hex": public_key.hex(),
        "message_hex": message.hex(),
        "signature_hex": signature.hex(),
    }


def transaction_fixtures():
    transfer = sample_payload()
    transfer["signature"] = Transaction.sign_payload(transfer, SENDER_PRIVATE)
    transfer_tx = Transaction(transfer)

    deploy = sample_payload(
        nonce=1,
        contract_code={
            "command": "deploy",
            "contract_id": "token",
            "pena_code": "x = 1\nfunction get() {\n return x\n}",
        },
        gas_limit=1000,
        gas_price=2,
    )
    deploy["signature"] = Transaction.sign_payload(deploy, SENDER_PRIVATE)
    deploy_tx = Transaction(deploy)

    return {
        "transfer": {
            "payload": transfer_tx.payload,
            "fee": transfer_tx.fee,
            "tx_id": transfer_tx.tx_id,
            "data": transfer_tx.data.decode("utf-8"),
            "serialize_hex": Transaction.serialize_message(transfer).hex(),
            "verify": Transaction.verify_transaction(transfer_tx.payload),
        },
        "deploy": {
            "payload": deploy_tx.payload,
            "fee": deploy_tx.fee,
            "tx_id": deploy_tx.tx_id,
            "data": deploy_tx.data.decode("utf-8"),
            "verify": Transaction.verify_transaction(deploy_tx.payload),
        },
        "invalid": [
            {"payload": {"nonce": 0}, "error": "chain_id"},
            {"payload": {"chain_id": CHAIN_ID, "gas_limit": 1}, "error": "gas fields"},
            {
                "payload": {"chain_id": CHAIN_ID, "validator": {"command": "deposit", "amount": 0}},
                "error": "positive integer",
            },
            {
                "payload": {
                    "chain_id": CHAIN_ID,
                    "governance": {
                        "command": "set_param", "name": "x", "value": 1, "approvals": [],
                    },
                },
                "error": "at least one approval",
            },
            {
                "payload": {
                    "chain_id": CHAIN_ID,
                    "contract_code": {"command": "deploy", "contract_id": "c"},
                    "gas_limit": 1,
                    "gas_price": 1,
                },
                "error": "deploy requires",
            },
        ],
    }


def sample_payload(**overrides):
    payload = {
        "chain_id": CHAIN_ID,
        "sender": SENDER_PUBLIC.hex(),
        "nonce": 0,
        "receiver": RECEIVER,
        "value": 100,
    }
    payload.update(overrides)
    return payload


def block_fixtures():
    tx1 = {"chain_id": CHAIN_ID, "sender": "aa", "nonce": 0, "receiver": RECEIVER, "value": 5}
    b0 = Block(
        index=0, previous_block_hash="0", validator="GENESIS_VALIDATOR",
        validator_signature="GENESIS_SIGNATURE", transactions=[GENESIS_MESSAGE],
        timestamp=0.0, chain_id=CHAIN_ID, state_root="root0",
    )
    b1 = Block(
        index=1, previous_block_hash=b0.current_block_hash, validator="v1",
        validator_signature="sig1", transactions=[tx1, "legacy"], timestamp=123.5,
        chain_id=CHAIN_ID, state_root="root1", round=2, reward_address=RECEIVER,
    )
    tampered = b1.to_dict()
    tampered["current_block_hash"] = "0" * 64
    return {
        "b0": {
            "dict": b0.to_dict(), "header": b0.to_header_dict(),
            "hash": b0.current_block_hash, "tx_root": b0.tx_root,
        },
        "b1": {
            "dict": b1.to_dict(), "header": b1.to_header_dict(),
            "hash": b1.current_block_hash, "tx_root": b1.tx_root,
        },
        "tampered": {"dict": tampered, "error": "hash mismatch"},
    }


def chain_fixtures():
    balances = {"0x" + "aa" * 20: 123456789, "0x" + "bb" * 20: 0}
    chain = Blockchain(genesis_balances=balances, chain_id=CHAIN_ID)

    median_chain = Blockchain(genesis_balances={}, chain_id=CHAIN_ID)
    timestamps = [0.0, 1.5, 2.5, 4.0]
    median_chain.chain = [
        Block(index=i, previous_block_hash="0", validator="v", validator_signature="s",
              transactions=[], timestamp=timestamp, chain_id=CHAIN_ID, state_root=None)
        for i, timestamp in enumerate(timestamps)
    ]
    return {
        "genesis_state_root": chain.genesis_state_root,
        "genesis_hash": chain.chain[0].current_block_hash,
        "genesis_allocations": chain.genesis_allocations,
        "parameters": chain.parameters,
        "genesis_parameters": chain.genesis_parameters,
        "median": [
            {"window": 11, "value": median_chain.median_time_past(11)},
            {"window": 2, "value": median_chain.median_time_past(2)},
            {"window": 3, "value": median_chain.median_time_past(3)},
        ],
        "next_fee_rate": chain.next_fee_rate(),
    }


STORE_STATE = {
    "balances": {"0xaa": 5},
    "nonces": {"0xaa": 1},
    "validators": {},
    "total_slashed": 0,
    "total_burned": 2,
    "base_fee": 3,
    "parameters": {"slash_bps": 5000},
}
STORE_STORAGE = {"data": {"x": 1}, "functions": {}, "contracts": {}}


def _store_blocks():
    b0 = Block(0, "0", "GENESIS_VALIDATOR", "GENESIS_SIGNATURE", [GENESIS_MESSAGE],
               timestamp=0.0, chain_id=CHAIN_ID, state_root="root0")
    b1 = Block(1, b0.current_block_hash, "v1", "sig1",
               [{"chain_id": CHAIN_ID, "sender": "aa", "nonce": 0, "value": 5}],
               timestamp=100.5, chain_id=CHAIN_ID, state_root="root1")
    b2 = Block(2, b1.current_block_hash, "v1", "sig2", [],
               timestamp=200.25, chain_id=CHAIN_ID, state_root="root2")
    return b0, b1, b2


def _dump_store(chain_store):
    memory = chain_store._kv
    return {
        "pairs": [[key.hex(), value.hex()] for key, value in sorted(memory._data.items())],
        "meta": {
            "head": chain_store.get_meta("head"),
            "schema_version": chain_store.get_meta("schema_version"),
            "pruned_below": chain_store.get_meta("pruned_below"),
            "snapshot_height": chain_store.get_meta("snapshot_height"),
        },
        "highest_height": chain_store.highest_height(),
        "count_blocks": chain_store.count_blocks(),
    }


def chainstore_fixtures():
    b0, b1, b2 = _store_blocks()

    store_a = ChainStore(":memory:", store=MemoryStore(":memory:"))
    store_a.append_block(b0, receipts=[], state=STORE_STATE, storage=STORE_STORAGE, head=0)
    store_a.append_block(
        b1, receipts=[{"tx_id": "tx1", "tx_index": 0}],
        state=STORE_STATE, storage=STORE_STORAGE, head=1,
    )
    store_a.append_block(b2, receipts=[], state=STORE_STATE, storage=STORE_STORAGE, head=2)
    result_a = _dump_store(store_a)
    result_a["state"] = canonical.dumps(store_a.load_state())
    result_a["storage"] = canonical.dumps(store_a.load_storage())
    result_a["receipts_at_1"] = canonical.dumps(store_a.receipts_for_block(1))
    result_a["tx_lookup"] = canonical.dumps(store_a.tx_lookup("tx1"))
    result_a["chain_len"] = len(store_a.load_chain())
    result_a["block1_hash"] = store_a.load_block(1).current_block_hash

    store_b = ChainStore(":memory:", store=MemoryStore(":memory:"))
    store_b.append_block(b0, receipts=[], head=0)
    store_b.append_block(b1, receipts=[{"tx_id": "tx1", "tx_index": 0}], head=1)
    store_b.append_block(b2, receipts=[], head=2)
    store_b.replace_chain(
        [b0, b1, b2], state=STORE_STATE, storage=STORE_STORAGE,
        receipts_by_index={1: [{"tx_id": "tx1", "tx_index": 0}]},
    )
    store_b.save_snapshot(1, b1.current_block_hash, STORE_STATE, STORE_STORAGE)
    store_b.save_snapshot(2, "0" * 64, STORE_STATE, STORE_STORAGE)
    removed = store_b.delete_mismatched_snapshots(
        lambda height: {1: b1.current_block_hash}.get(height)
    )
    store_b.delete_blocks_after(1)
    store_b.prune_blocks_below(2)
    result_b = _dump_store(store_b)
    result_b["snapshots_removed"] = removed
    result_b["snapshots"] = store_b.list_snapshots()
    result_b["latest_snapshot_height"] = store_b.load_latest_snapshot()["height"]

    return {"a": result_a, "b": result_b}


SANVM_PROGRAMS = [
    ("arith", "pena", "x = (3 + 5) * 2 - 4 / 2 % 3\nprint(x)"),
    ("for_loop", "pena", "for i, 0 -> 5 {\n print(i)\n}"),
    (
        "while_break",
        "pena",
        "x = 0\nwhile (x < 10) {\n x = x + 1\n if (x == 4) {\n  continue\n }\n"
        " if (x == 7) {\n  break\n }\n print(x)\n}",
    ),
    (
        "fib",
        "pena",
        "function fib(n) {\n if (n < 2) {\n  return n\n }\n"
        " return fib(n - 1) + fib(n - 2)\n}\nprint(fib(10))",
    ),
    ("lists", "pena", "mylist := [1, 2, 3]\nprint(mylist[1])\nprint(mylist)"),
    (
        "dicts",
        "pena",
        'balances := {}\nbalances["alice"] = 100\nprint(balances["alice"])\n'
        'print(balances["bob"])',
    ),
    ("strings", "pena", 'print("hello " + "world")'),
    ("negative_math", "pena", "print(-7 / 2)\nprint(-7 % 2)\nprint(7 % -2)"),
    (
        "logic",
        "pena",
        "x = 3\nprint(x > 1 && x < 5)\nprint(!(x == 3))\nprint(x != 4 || x == 99)",
    ),
    ("floats", "pena", "x = 1.5 * 2\nprint(x)\nprint(1 + 0.5)"),
    ("bigint", "pena", "print(123456789012345678901234567890 + 1)"),
    (
        "inline_asm",
        "pena",
        "x = 41\nasm {\n GET x\n PUSH 1\n ADD\n SET y\n}\nprint(y)",
    ),
    (
        "pure_asm",
        "asm",
        "PUSH 0\nSET total\nPUSH 1\nSET n\n.loop:\nGET n\nPUSH 11\nLT\nJZ .done\n"
        "GET total\nGET n\nADD\nSET total\nGET n\nPUSH 1\nADD\nSET n\nJMP .loop\n"
        ".done:\nGET total\nPRINT\nHALT",
    ),
    (
        "func_asm",
        "asm",
        "FUNC add(a, b) {\n GET a\n GET b\n ADD\n RET\n}\nPUSH 10\nPUSH 20\n"
        "PUSH add\nPUSH 2\nCALL_FUNC\nPRINT\nHALT",
    ),
    (
        "collections_asm",
        "asm",
        "PUSH []\nSET items\nPUSH 1\nLIST_APPEND items\nPUSH 2\nLIST_APPEND items\n"
        "PUSH 3\nLIST_APPEND items\nLIST_LEN items\nPRINT\nPUSH 1\nLIST_GET items\n"
        "PRINT\nPUSH 2\nLIST_REMOVE items\nLIST_LEN items\nPRINT\nPUSH {}\nSET book\n"
        "PUSH gold\nPUSH 7\nDICT_SET book\nPUSH gold\nDICT_GET book\nPRINT\n"
        "DICT_KEYS book\nPRINT\nHALT",
    ),
]


def sanvm_program_fixtures():
    from SANVM.pena_parser import compile_pena
    from SANVM.Storage import Storage
    from SANVM.VM import SANVirtualMachine

    programs = []
    for name, language, source in SANVM_PROGRAMS:
        bytecode = compile_pena(source, language)
        vm = SANVirtualMachine(Storage(), verbose=False)
        vm.run(bytecode)
        programs.append(
            {
                "name": name,
                "language": language,
                "source": source,
                "bytecode": bytecode,
                "bytecode_json": canonical.dumps(bytecode),
                "logs": canonical.dumps(vm.logs),
                "stack": canonical.dumps(vm.stack),
                "gas_used": vm.gas_used,
            }
        )
    return programs


def sanvm_contract_fixtures():
    from SANVM.pena_parser import PenaParser
    from SANVM.Storage import Storage
    from SANVM.VM import SANVirtualMachine

    vm = SANVirtualMachine(Storage(), verbose=False)
    token_source = (
        "balance := {}\n"
        "function set(owner, amount) {\n  balance[owner] = amount\n}\n"
        "function get(owner) {\n  return balance[owner]\n}\n"
    )
    vm.deploy_contract("token", PenaParser().parse(token_source))
    set_result = vm.call_contract_function("token", "set", ["alice", 100])
    set_logs = canonical.dumps(vm.contract_manager.last_logs)
    set_gas = vm.contract_manager.last_gas_used
    get_result = vm.call_contract_function("token", "get", ["alice"])
    token_storage = canonical.dumps(vm.storage.contracts["token"]["storage"])

    boom_source = (
        "x = 1\n"
        "function fail() {\n  x = 2\n  y = 1 / 0\n  return 1\n}\n"
        "function get() {\n  return x\n}\n"
    )
    vm.deploy_contract("boom", PenaParser().parse(boom_source))
    failure = ""
    try:
        vm.call_contract_function("boom", "fail", [])
    except Exception as exc:  # noqa: BLE001
        failure = type(exc).__name__
    rollback_result = vm.call_contract_function("boom", "get", [])

    duplicate = ""
    try:
        vm.deploy_contract("token", PenaParser().parse(token_source))
    except Exception as exc:  # noqa: BLE001
        duplicate = type(exc).__name__

    return {
        "token_source": token_source,
        "boom_source": boom_source,
        "set_result": set_result,
        "set_logs": set_logs,
        "set_gas": set_gas,
        "get_result": get_result,
        "token_storage": token_storage,
        "failure": failure,
        "rollback_result": rollback_result,
        "duplicate": duplicate,
    }


def sanvm_limit_fixtures():
    from SANVM.pena_parser import PenaParser
    from SANVM.Storage import Storage
    from SANVM.VM import SANVirtualMachine

    results = []
    cases = [
        ("steps", lambda: SANVirtualMachine(Storage(), max_steps=500, verbose=False).run(
            PenaParser().parse("while (1) { }"))),
        ("stack", lambda: SANVirtualMachine(Storage(), max_stack=8, verbose=False).run(
            [0x01, 1] * 20)),
        ("gas", lambda: SANVirtualMachine(Storage(), gas_limit=10, verbose=False).run(
            PenaParser().parse("x = 1\nwhile (x < 1000) {\n x = x + 1\n}"))),
        ("divzero", lambda: SANVirtualMachine(Storage(), verbose=False).run(
            PenaParser().parse("x = 1 / 0"))),
    ]
    for name, runner in cases:
        try:
            runner()
            results.append({"case": name, "error": None})
        except Exception as exc:  # noqa: BLE001
            results.append({"case": name, "error": type(exc).__name__})
    return results


def sanvm_error_fixtures():
    from SANVM.pena_parser import PenaParser
    from SANVM.asm import assemble

    errors = []
    for source in [
        "function (a) {\n return 1\n}",
        "for i, a -> b {\n}",
        "break",
        "continue",
        "asm {\nPUSH 1",
        "nil = [1, 2",
    ]:
        try:
            PenaParser().parse(source)
            errors.append({"kind": "pena", "source": source, "error": None})
        except Exception as exc:  # noqa: BLE001
            errors.append({"kind": "pena", "source": source, "error": type(exc).__name__})
    for source in ["BOGUS 1", "PUSH", "JMP .nowhere", "PUSH 1\n}"]:
        try:
            assemble(source)
            errors.append({"kind": "asm", "source": source, "error": None})
        except Exception as exc:  # noqa: BLE001
            errors.append({"kind": "asm", "source": source, "error": type(exc).__name__})
    return errors


def sanvm_asm_fixtures():
    from SANVM.asm import assemble, disassemble

    sources = [source for _, language, source in SANVM_PROGRAMS if language == "asm"]
    result = []
    for source in sources:
        bytecode = assemble(source)
        result.append(
            {
                "source": source,
                "bytecode_json": canonical.dumps(bytecode),
                "disassembly": disassemble(bytecode),
            }
        )
    return result


def main() -> int:
    fixture = {
        "canonical": canonical_fixtures(),
        "float_repr": float_fixtures(),
        "address": address_fixtures(),
        "merkle": merkle_fixtures(),
        "state": state_fixtures(),
        "economics": economics_fixtures(),
        "identity": identity_fixtures(),
        "transaction": transaction_fixtures(),
        "blocks": block_fixtures(),
        "chain": chain_fixtures(),
        "chainstore": chainstore_fixtures(),
        "sanvm_programs": sanvm_program_fixtures(),
        "sanvm_contracts": sanvm_contract_fixtures(),
        "sanvm_limits": sanvm_limit_fixtures(),
        "sanvm_errors": sanvm_error_fixtures(),
        "sanvm_asm": sanvm_asm_fixtures(),
    }
    target = ROOT / "internal" / "parity" / "testdata" / "foundation.json"
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(json.dumps(fixture, indent=2, ensure_ascii=False), encoding="utf-8")
    print(f"wrote {target}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
