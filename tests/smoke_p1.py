"""P1 verification: money/nonce, VM/PENA, contracts, block validation, votes.

Run from the repository root:

    python tests/smoke_p1.py
"""
import asyncio
import contextlib
import io
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

from helpers import (  # noqa: E402
    CHAIN_ID,
    SAN_BASE,
    FakePeerServer,
    check,
    free_port,
    make_config,
    sample_address,
    seal_block,
    setup_identity,
    signed_payload,
    summary,
)

from blockchain import crypto  # noqa: E402
from blockchain.Block import Block  # noqa: E402
from blockchain.economics import (  # noqa: E402
    MAX_FEE_PER_BYTE,
    MIN_FEE_PER_BYTE,
    fee_rate_for_transaction_count,
    san_to_units,
)
from blockchain.identity import NodeIdentity  # noqa: E402
from blockchain.Transaction import Transaction  # noqa: E402
from network.Node import Node  # noqa: E402
from SANVM.OpCode import OpCode  # noqa: E402
from SANVM.pena_parser import PenaParser  # noqa: E402
from SANVM.Storage import Storage  # noqa: E402
from SANVM.VM import SANVirtualMachine  # noqa: E402


def economics_checks():
    print("\n=== P1-8/9 economics ===")
    check("1 SAN = 10^8 units", san_to_units(1) == SAN_BASE)
    check("0.01 SAN exact", san_to_units("0.01") == SAN_BASE // 100)
    check("too many decimals rejected", _raises(san_to_units, "0.000000001"))
    check(
        "fee rate grows with congestion",
        fee_rate_for_transaction_count(0) == MIN_FEE_PER_BYTE
        and fee_rate_for_transaction_count(100) == MIN_FEE_PER_BYTE * 2
        and fee_rate_for_transaction_count(10 ** 9) == MAX_FEE_PER_BYTE,
    )


def _raises(func, *args, **kwargs):
    try:
        func(*args, **kwargs)
    except Exception:  # noqa: BLE001
        return True
    return False


def transaction_checks(public_key, private_key):
    print("\n=== P1-5 transaction signatures ===")
    payload = {
        "chain_id": CHAIN_ID,
        "sender": public_key.hex(),
        "nonce": 0,
        "receiver": sample_address("b0"),
        "value": 1,
    }
    payload["signature"] = Transaction.sign_payload(payload, private_key)

    check("valid signature verifies", Transaction.verify_transaction(payload) is True)

    tampered = dict(payload)
    tampered["value"] = 999
    check("tampered payload rejected", Transaction.verify_transaction(tampered) is False)

    unsigned = {"sender": public_key.hex(), "nonce": 0}
    check("unsigned transaction rejected", Transaction.verify_transaction(unsigned) is False)

    bad_hex = {"sender": "zz", "signature": "zz"}
    check("malformed hex rejected without raising", Transaction.verify_transaction(bad_hex) is False)

    fee_a = Transaction.expected_fee(payload, MIN_FEE_PER_BYTE)
    fee_b = Transaction.expected_fee(payload, MIN_FEE_PER_BYTE)
    check("fee calculation deterministic", fee_a == fee_b and fee_a > 0)


def vm_checks():
    print("\n=== P1-11/12/13 VM opcodes ===")

    vm = SANVirtualMachine(Storage())
    vm.stack = [1, 2, 3]
    vm.dup()
    check("DUP copies the top item", vm.stack == [1, 2, 3, 3], str(vm.stack))

    vm = SANVirtualMachine(Storage())
    vm.run([
        OpCode.PUSH.value, "x", OpCode.PUSH.value, 5, OpCode.SET.value,
        OpCode.PUSH.value, "x", OpCode.DELETE.value,
        OpCode.PUSH.value, "x", OpCode.HAS.value,
    ])
    check("DELETE removes a variable", vm.stack[-1] == 0 and not vm.storage.has_var("x"))

    vm = SANVirtualMachine(Storage())
    vm.run([
        OpCode.PUSH.value, 4, OpCode.CALL.value, OpCode.HALT.value,
        OpCode.PUSH.value, 42, OpCode.RET.value,
    ])
    check("CALL/RET frame returns to caller", vm.stack == [42], str(vm.stack))

    vm = SANVirtualMachine(Storage(), max_steps=500)
    bytecode = PenaParser().parse("while (1) { }")
    check("infinite loop hits the step limit", _raises(vm.run, bytecode))

    vm = SANVirtualMachine(Storage(), max_stack=8)
    check(
        "stack limit enforced",
        _raises(vm.run, [OpCode.PUSH.value, 1] * 20),
    )


def pena_checks():
    print("\n=== P1-14/15/16/21/22 PENA language ===")
    programs = {
        "strings": ('print("Hello " + "World")', ["Hello World"]),
        "arithmetic": ("x = (3 + 5) * 2\nprint(x)", ["16"]),
        "unary-minus": ("x = -5\nprint(x + 2)", ["-3"]),
        "if-else-if": (
            'x = 7\nif (x > 10) {\n print("big")\n}\nelse if (x > 5) {\n print("medium")\n}\nelse {\n print("small")\n}',
            ["medium"],
        ),
        "while": ("x = 0\nwhile (x < 3) {\n print(x)\n x = x + 1\n}", ["0", "1", "2"]),
        "for-break-continue": (
            "for i, 0 -> 10 {\n if (i == 2) {\n  continue\n }\n if (i == 5) {\n  break\n }\n print(i)\n}",
            ["0", "1", "3", "4"],
        ),
        "functions": (
            "function add(a, b) {\n return a + b\n}\nprint(add(10, 20))",
            ["30"],
        ),
        "recursion": (
            "function fib(n) {\n if (n < 2) {\n  return n\n }\n return fib(n - 1) + fib(n - 2)\n}\nprint(fib(10))",
            ["55"],
        ),
        "list": ("mylist := [1, 2, 3]\nprint(mylist)\nprint(mylist[1])", ["[1, 2, 3]", "2"]),
        "dict": ('mydict := {}\nmydict["a"] = 5\nprint(mydict["a"])', ["5"]),
        "logical": ('x = 1\nif (x == 1 && x != 2) {\n print("ok")\n}', ["ok"]),
        "empty-equality": (
            'owner_of := {}\no = owner_of[404]\nif (o == "") {\n print("not minted")\n}',
            ["not minted"],
        ),
    }

    for name, (source, expected) in programs.items():
        buffer = io.StringIO()
        try:
            bytecode = PenaParser().parse(source)
            with contextlib.redirect_stdout(buffer):
                SANVirtualMachine(Storage()).run(bytecode)
            output = buffer.getvalue().splitlines()
        except Exception as exc:  # noqa: BLE001
            check(f"PENA {name}", False, f"{type(exc).__name__}: {exc}")
            continue
        check(f"PENA {name}", output == expected, f"got {output}, expected {expected}")


def contract_checks():
    print("\n=== P1-17/18/19 contracts ===")
    vm = SANVirtualMachine(Storage())

    vm.deploy_contract("token", PenaParser().parse(
        "balance := {}\n"
        "function init(owner, amount) {\n  balance[owner] = amount\n}\n"
        "function transfer(from, to, amount) {\n"
        "  current = balance[from]\n"
        "  if (current < amount) {\n    return 0\n  }\n"
        "  balance[from] = current - amount\n"
        "  to_balance = balance[to]\n"
        "  balance[to] = to_balance + amount\n"
        "  return 1\n"
        "}\n"
        "function balanceOf(owner) {\n  return balance[owner]\n}\n"
    ))
    vm.call_contract_function("token", "init", ["alice", 100])
    vm.call_contract_function("token", "transfer", ["alice", "bob", 40])
    check(
        "contract state updated",
        vm.call_contract_function("token", "balanceOf", ["alice"]) == 60
        and vm.call_contract_function("token", "balanceOf", ["bob"]) == 40,
    )

    vm.deploy_contract("counter", PenaParser().parse("x = 1\nfunction get() {\n return x\n}"))
    check(
        "contract storage isolation",
        vm.call_contract_function("counter", "get", []) == 1
        and vm.call_contract_function("token", "balanceOf", ["alice"]) == 60,
    )

    vm.deploy_contract("boom", PenaParser().parse(
        "x = 1\n"
        "function fail() {\n  x = 2\n  y = 1 / 0\n  return 1\n}\n"
        "function get() {\n  return x\n}\n"
    ))
    check("failing call raises", _raises(vm.call_contract_function, "boom", "fail", []))
    check(
        "failing call is rolled back",
        vm.call_contract_function("boom", "get", []) == 1,
    )

    check(
        "duplicate contract id rejected",
        _raises(vm.deploy_contract, "token", []),
    )
    huge_id = "1" + "0" * 80
    check("invalid (too large) contract id rejected", _raises(vm.deploy_contract, huge_id, []))
    check("unknown contract rejected", _raises(vm.call_contract_function, "ghost", "f", []))

    # Real examples shipped with the repository.
    sanrc20 = SANVirtualMachine(Storage())
    sanrc20.deploy_contract(
        "sanrc20", PenaParser().parse(open("PENA/examples/SANRC20/SANRC20.pena").read())
    )
    sanrc20.call_contract_function("sanrc20", "init", ["SAN", "SANRC20", 18, 1000, "owner"])
    sanrc20.call_contract_function("sanrc20", "transfer", ["owner", "alice", 100])
    check(
        "SANRC20 example runs end to end",
        sanrc20.call_contract_function("sanrc20", "balanceOf", ["alice"]) == 100
        and sanrc20.call_contract_function("sanrc20", "transfer", ["alice", "owner", 1000]) == 0,
    )

    nft = SANVirtualMachine(Storage())
    nft.deploy_contract(
        "nft", PenaParser().parse(open("PENA/examples/SANRC721/SANRC721.pena").read())
    )
    nft.call_contract_function("nft", "init", ["NFT", "NFT", "owner"])
    nft.call_contract_function("nft", "mint", ["owner", "alice", 1])
    nft.call_contract_function("nft", "approve", ["alice", "bob", 1])
    nft.call_contract_function("nft", "transferFrom", ["bob", "alice", "carol", 1])
    check(
        "SANRC721 example runs end to end",
        nft.call_contract_function("nft", "ownerOf", [1]) == "carol"
        and nft.call_contract_function("nft", "ownerOf", [2]) == "",
    )

    amm = SANVirtualMachine(Storage())
    amm.deploy_contract("amm", PenaParser().parse(open("PENA/examples/AMM/AMM.pena").read()))
    amm.call_contract_function("amm", "init", ["SANRC20", "USDT", 30, "owner"])
    first = amm.call_contract_function("amm", "addLiquidity", ["alice", 1000, 1000])
    second = amm.call_contract_function("amm", "addLiquidity", ["bob", 500, 500])
    out = amm.call_contract_function("amm", "swap0For1", ["bob", 100])
    check(
        "AMM example runs end to end",
        first == 2000 and second == 1000 and 90 <= out <= 100,
        f"first={first} second={second} out={out}",
    )


async def node_checks(public_key, private_key):
    print("\n=== P1-2/6/7/9/10 node validation ===")
    node = Node(make_config())
    await node.start()

    def fresh_payload(nonce, value=1, receiver=None):
        return signed_payload(
            public_key, private_key, nonce, receiver=receiver or sample_address("b0"), value=value
        )

    check("unsigned transaction rejected by mempool", await _raises_async(node, {"sender": "0x1", "nonce": 0}))
    check("bad nonce rejected", await _raises_async(node, fresh_payload(5)))
    check("negative value rejected", await _raises_async(node, fresh_payload(0, value=-10)))
    check(
        "insufficient balance rejected",
        await _raises_async(node, fresh_payload(0, value=2_000_000)),
    )

    tampered = fresh_payload(0)
    tampered["value"] = 500
    check("tampered signature rejected by mempool", await _raises_async(node, tampered))

    result = await node.submit_transaction(fresh_payload(0))
    check("valid transaction committed", result["status"] == "committed", json.dumps(result))

    replay = dict(fresh_payload(0))
    check("replayed transaction rejected", await _raises_async(node, replay))

    # --- Block level tampering ----------------------------------------- #
    tip = node.blockchain.tip
    rate = node.blockchain.next_fee_rate()

    def pooled(nonce, value=1, receiver=None):
        return Transaction(
            signed_payload(
                public_key,
                private_key,
                nonce,
                receiver=receiver or sample_address("b0"),
                value=value,
            ),
            rate,
        ).to_dict()

    good = pooled(1)

    def make_block(transactions):
        block = Block(
            index=tip.index + 1,
            previous_block_hash=tip.current_block_hash,
            validator=node.get_public_key() or "UNSIGNED_VALIDATOR",
            validator_signature=None,
            transactions=transactions,
            chain_id=CHAIN_ID,
        )
        return seal_block(node, block) or block

    block = make_block([good])
    check("valid block verifies", node.verify_block(block) is True)

    wrong_hash = make_block([good])
    wrong_hash.current_block_hash = "deadbeef"
    check("tampered block hash rejected", node.verify_block(wrong_hash) is False)

    bad_fee = dict(good)
    bad_fee["fee"] = 1
    check("wrong fee rejected", node.verify_block(make_block([bad_fee])) is False)

    wrong_nonce = pooled(9)
    check("wrong nonce rejected", node.verify_block(make_block([wrong_nonce])) is False)

    other_key = crypto.generate_keypair()[1]
    forged = make_block([good])
    forged.validator_signature = crypto.sign(
        forged.current_block_hash.encode(), other_key
    ).hex()
    check("forged validator signature rejected", node.verify_block(forged) is False)

    unsigned_block = make_block([good])
    unsigned_block.validator_signature = None
    check("unsigned block rejected", node.verify_block(unsigned_block) is False)

    # --- Atomicity: second transaction cannot be afforded ----------------- #
    balances_before = dict(node.blockchain.SAN)
    nonces_before = dict(node.blockchain.nonces)
    chain_before = len(node.blockchain.chain)

    big1 = pooled(1, value=600_000, receiver=sample_address("aa"))
    big2 = pooled(2, value=600_000, receiver=sample_address("cc"))
    atomic_block = make_block([big1, big2])
    check("block with unaffordable tx rejected", node._commit_block(atomic_block) is False)
    check(
        "rejected block left no partial state",
        node.blockchain.SAN == balances_before
        and node.blockchain.nonces == nonces_before
        and len(node.blockchain.chain) == chain_before,
    )

    await node.stop()


async def _raises_async(node, payload):
    try:
        await node.submit_transaction(payload)
    except (ValueError, TypeError):
        return True
    return False


async def malicious_vote_checks(public_key, private_key):
    print("\n=== P1-4 signed controller votes ===")

    node = Node(make_config(block_threshold_fee=1_000_000))
    await node.start()
    await node.submit_transaction(
        signed_payload(public_key, private_key, 0, receiver=sample_address("b1"), value=1)
    )
    block = node._build_block()

    port = free_port()
    mode = {"value": "unsigned"}
    fake_identity = NodeIdentity.generate()

    async def responder(recv, send):
        await recv()
        response = {
            "type": "BLOCK_VOTE_RESPONSE",
            "chain_id": CHAIN_ID,
            "approved": True,
            "block_hash": block.current_block_hash,
        }
        if mode["value"] == "forged":
            response["public_key"] = fake_identity.public_key_hex
            response["signature"] = "00"
        elif mode["value"] == "signed":
            response["public_key"] = node.get_public_key()
            signature = node._sign_vote({
                "type": response["type"],
                "chain_id": response["chain_id"],
                "approved": response["approved"],
                "block_hash": response["block_hash"],
            })
            response["signature"] = signature
        await send(response)

    server = FakePeerServer(fake_identity, responder, port)
    await server.start()
    controller = {"host": "127.0.0.1", "api_port": port, "p2p_port": port,
                  "peer_port": port, "controller_port": port}

    mode["value"] = "unsigned"
    check("unsigned vote rejected", await node._request_block_vote(controller, block) is False)

    mode["value"] = "forged"
    check("forged vote signature rejected", await node._request_block_vote(controller, block) is False)

    mode["value"] = "signed"
    check("valid signed vote accepted", await node._request_block_vote(controller, block) is True)

    await server.stop()
    await node.stop()


async def run_async():
    public_key, private_key = PUBLIC_KEY, PRIVATE_KEY
    await node_checks(public_key, private_key)
    await malicious_vote_checks(public_key, private_key)


if __name__ == "__main__":
    PUBLIC_KEY, PRIVATE_KEY = setup_identity()
    economics_checks()
    transaction_checks(PUBLIC_KEY, PRIVATE_KEY)
    vm_checks()
    pena_checks()
    contract_checks()
    asyncio.run(run_async())
    sys.exit(summary())
