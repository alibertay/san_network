"""Fast unit tests: money, transactions, blocks, VM/PENA, contracts, identity."""
import asyncio
import json
import sys
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

from helpers import CHAIN_ID, address_of, free_port, sample_address, seal_block  # noqa: E402

from blockchain import crypto  # noqa: E402
from blockchain.Block import SCHEMA_VERSION, Block  # noqa: E402
from blockchain.Blockchain import Blockchain  # noqa: E402
from blockchain.economics import (  # noqa: E402
    MAX_FEE_PER_BYTE,
    MIN_FEE_PER_BYTE,
    SAN_BASE,
    fee_rate_for_transaction_count,
    san_to_units,
    units_to_san,
)
from blockchain.identity import IdentityError, NodeIdentity  # noqa: E402
from blockchain.Transaction import Transaction  # noqa: E402
from network.config import NodeConfig  # noqa: E402
from network.Node import Node  # noqa: E402
from SANVM.pena_parser import PenaParser  # noqa: E402
from SANVM.Storage import Storage  # noqa: E402
from SANVM.VM import SANVirtualMachine, StackLimitExceeded, StepLimitExceeded  # noqa: E402

# ---------------------------------------------------------------------- #
# Economics
# ---------------------------------------------------------------------- #

def test_money_conversions():
    assert san_to_units(1) == SAN_BASE
    assert san_to_units("0.01") == SAN_BASE // 100
    assert units_to_san(SAN_BASE) == 1
    with pytest.raises(ValueError):
        san_to_units("0.000000001")
    with pytest.raises(ValueError):
        san_to_units(True)


def test_fee_rate_grows_with_congestion():
    assert fee_rate_for_transaction_count(0) == MIN_FEE_PER_BYTE
    assert fee_rate_for_transaction_count(100) == MIN_FEE_PER_BYTE * 2
    assert fee_rate_for_transaction_count(10 ** 9) == MAX_FEE_PER_BYTE


# ---------------------------------------------------------------------- #
# Transactions and blocks
# ---------------------------------------------------------------------- #

def test_transaction_signature_and_fee():
    public_key, private_key = crypto.generate_keypair()
    payload = {
        "chain_id": CHAIN_ID,
        "sender": public_key.hex(),
        "nonce": 0,
        "receiver": sample_address("b0"),
        "value": 1,
    }
    payload["signature"] = Transaction.sign_payload(payload, private_key)

    assert Transaction.verify_transaction(payload) is True
    assert Transaction.verify_transaction({**payload, "value": 99}) is False
    assert Transaction.verify_transaction({"sender": "zz", "signature": "zz"}) is False

    fee_a = Transaction.expected_fee(payload, MIN_FEE_PER_BYTE)
    fee_b = Transaction.expected_fee(payload, MIN_FEE_PER_BYTE)
    assert fee_a == fee_b > 0

    stored = Transaction(payload, MIN_FEE_PER_BYTE)
    assert stored.fee == fee_a
    assert stored.payload["fee"] == fee_a
    assert stored.tx_id


def test_block_hash_is_deterministic_and_versioned():
    genesis_a = Blockchain().tip
    genesis_b = Blockchain().tip
    assert genesis_a.current_block_hash == genesis_b.current_block_hash

    restored = Block.from_dict(json.loads(json.dumps(genesis_a.to_dict())))
    assert restored.current_block_hash == genesis_a.current_block_hash
    assert genesis_a.to_dict()["version"] == SCHEMA_VERSION

    with pytest.raises(ValueError):
        Block.from_dict({**genesis_a.to_dict(), "version": SCHEMA_VERSION + 1})


def test_block_from_dict_rejects_tampering():
    block = Block(0, "0", "v", "s", [{"x": 1}], timestamp=0.0)
    tampered = {**block.to_dict(), "previous_block_hash": "deadbeef"}
    with pytest.raises(ValueError):
        Block.from_dict(tampered)


# ---------------------------------------------------------------------- #
# Faz A: chain_id, addresses, median time
# ---------------------------------------------------------------------- #

def test_address_derivation_and_validation():
    from blockchain.address import AddressError, address_from_public_key, normalize_address

    public_key, _ = crypto.generate_keypair()
    address = address_from_public_key(public_key)
    assert address.startswith("0x") and len(address) == 42
    assert address == address_from_public_key(public_key.hex())
    assert normalize_address(address.upper()) == address

    with pytest.raises(AddressError):
        normalize_address("0x1234")
    with pytest.raises(AddressError):
        address_from_public_key("not-hex")


def test_chain_id_is_bound_into_transaction_signatures():
    public_key, private_key = crypto.generate_keypair()
    payload = {
        "chain_id": "chain-a",
        "sender": public_key.hex(),
        "nonce": 0,
        "receiver": sample_address("aa"),
        "value": 1,
    }
    payload["signature"] = Transaction.sign_payload(payload, private_key)

    # Signature covers the chain id: changing it invalidates the signature.
    assert Transaction.verify_transaction(payload) is True
    assert Transaction.verify_transaction({**payload, "chain_id": "chain-b"}) is False

    # A node on another chain rejects the (otherwise valid) transaction.
    node = Node(NodeConfig(chain_id="chain-b"), identity=NodeIdentity.generate())
    with pytest.raises(ValueError):
        asyncio_run(node.submit_transaction(payload))

    # Blocks are bound to the chain too.
    block = Blockchain(chain_id="chain-a").tip
    assert block.chain_id == "chain-a"
    assert Blockchain(chain_id="chain-a").tip.current_block_hash != Blockchain(
        chain_id="chain-b"
    ).tip.current_block_hash


def asyncio_run(coro):
    import asyncio

    return asyncio.run(coro)


def test_median_time_rule():
    public_key, private_key = crypto.generate_keypair()
    allocations = {public_key.hex(): 1_000_000 * SAN_BASE}
    identity = NodeIdentity(private_key=private_key, public_key=public_key)
    node = Node(
        NodeConfig(
            chain_id=CHAIN_ID,
            genesis_allocations=allocations,
            require_state_root=False,
        ),
        identity=identity,
    )

    tx = {
        "chain_id": CHAIN_ID,
        "sender": public_key.hex(),
        "nonce": 0,
        "receiver": sample_address("11"),
        "value": 1,
    }
    tx["signature"] = Transaction.sign_payload(payload=tx, private_key=private_key)
    rate = node.blockchain.next_fee_rate()
    transaction = Transaction(tx, rate).to_dict()

    def block_at(timestamp):
        block = Block(
            index=1,
            previous_block_hash=node.blockchain.tip.current_block_hash,
            validator=public_key.hex(),
            validator_signature=None,
            transactions=[transaction],
            timestamp=timestamp,
            chain_id=CHAIN_ID,
        )
        block.validator_signature = identity.sign_hex(block.current_block_hash.encode())
        return block

    # Not newer than the median (genesis 0) is rejected.
    assert node.verify_block(block_at(0.0)) is False
    # Too far in the future is rejected.
    assert node.verify_block(block_at(time.time() + 10_000)) is False
    # A sane timestamp passes.
    assert node.verify_block(block_at(time.time())) is True


def test_handshake_requires_matching_chain_and_valid_signature():
    identity = NodeIdentity.generate()
    node = Node(NodeConfig(chain_id="chain-a"), identity=identity)

    hello = node._hello_payload("HELLO")
    assert node._verify_hello(hello) is True

    # Wrong chain id is rejected.
    other = Node(NodeConfig(chain_id="chain-b"), identity=identity)
    assert other._verify_hello(hello) is False

    # Tampered payload fails signature verification.
    assert node._verify_hello({**hello, "public_key": NodeIdentity.generate().public_key_hex}) is False
    # Stale handshake is rejected.
    assert node._verify_hello({**hello, "timestamp": time.time() - 10_000}) is False


# ---------------------------------------------------------------------- #
# VM and PENA
# ---------------------------------------------------------------------- #

PROGRAMS = [
    ("print(\"Hello \" + \"World\")", ["Hello World"]),
    ("x = (3 + 5) * 2\nprint(x)", ["16"]),
    ("x = -5\nprint(x + 2)", ["-3"]),
    ('x = 7\nif (x > 10) {\n print("big")\n}\nelse {\n print("small")\n}', ["small"]),
    ("x = 0\nwhile (x < 3) {\n print(x)\n x = x + 1\n}", ["0", "1", "2"]),
    (
        "for i, 0 -> 10 {\n if (i == 2) {\n  continue\n }\n if (i == 5) {\n  break\n }\n print(i)\n}",
        ["0", "1", "3", "4"],
    ),
    ("function add(a, b) {\n return a + b\n}\nprint(add(10, 20))", ["30"]),
    (
        "function fib(n) {\n if (n < 2) {\n  return n\n }\n return fib(n - 1) + fib(n - 2)\n}\nprint(fib(10))",
        ["55"],
    ),
    ('mylist := [1, 2, 3]\nprint(mylist[1])\nprint(mylist)', ["2", "[1, 2, 3]"]),
    ('mydict := {}\nmydict["a"] = 5\nprint(mydict["a"])', ["5"]),
    ('x = 1\nif (x == 1 && x != 2) {\n print("ok")\n}', ["ok"]),
    (
        'owner_of := {}\no = owner_of[404]\nif (o == "") {\n print("not minted")\n}',
        ["not minted"],
    ),
]


@pytest.mark.parametrize("source,expected", PROGRAMS)
def test_pena_programs(source, expected, capsys):
    SANVirtualMachine(Storage()).run(PenaParser().parse(source))
    assert capsys.readouterr().out.splitlines() == expected


def test_vm_limits():
    with pytest.raises(StepLimitExceeded):
        SANVirtualMachine(Storage(), max_steps=500).run(PenaParser().parse("while (1) { }"))

    with pytest.raises(StackLimitExceeded):
        SANVirtualMachine(Storage(), max_stack=8).run([0x01, 1] * 20)


def test_vm_dup_delete_and_call_frames():
    vm = SANVirtualMachine(Storage())
    vm.stack = [1, 2, 3]
    vm.dup()
    assert vm.stack == [1, 2, 3, 3]

    vm = SANVirtualMachine(Storage())
    vm.run([0x01, "x", 0x01, 5, 0x1D, 0x01, "x", 0x1F, 0x01, "x", 0x20])
    assert vm.stack[-1] == 0 and not vm.storage.has_var("x")

    vm = SANVirtualMachine(Storage())
    vm.run([0x01, 4, 0x17, 0xFF, 0x01, 42, 0x18])
    assert vm.stack == [42]


# ---------------------------------------------------------------------- #
# Contracts
# ---------------------------------------------------------------------- #

def test_contract_isolation_and_rollback():
    vm = SANVirtualMachine(Storage())
    vm.deploy_contract(
        "token",
        PenaParser().parse(
            "balance := {}\n"
            "function set(owner, amount) {\n  balance[owner] = amount\n}\n"
            "function get(owner) {\n  return balance[owner]\n}\n"
        ),
    )
    vm.deploy_contract("other", PenaParser().parse("x = 1\nfunction get() {\n return x\n}"))
    vm.call_contract_function("token", "set", ["alice", 100])
    assert vm.call_contract_function("token", "get", ["alice"]) == 100
    assert vm.call_contract_function("other", "get", []) == 1

    vm.deploy_contract(
        "boom",
        PenaParser().parse(
            "x = 1\nfunction fail() {\n  x = 2\n  y = 1 / 0\n  return 1\n}\nfunction get() {\n  return x\n}\n"
        ),
    )
    with pytest.raises(Exception):
        vm.call_contract_function("boom", "fail", [])
    assert vm.call_contract_function("boom", "get", []) == 1

    with pytest.raises(ValueError):
        vm.deploy_contract("token", [])


# ---------------------------------------------------------------------- #
# Identity and peer records
# ---------------------------------------------------------------------- #

def test_identity_file_roundtrip(tmp_path):
    identity = NodeIdentity.generate()
    path = tmp_path / "key.json"
    identity.save(str(path))
    loaded = NodeIdentity.from_file(str(path))
    assert loaded.public_key == identity.public_key
    assert loaded.private_key == identity.private_key

    with pytest.raises(IdentityError):
        NodeIdentity.from_file(str(tmp_path / "missing.json"))

    broken = tmp_path / "broken.json"
    broken.write_text('{"private_key": "zz"}')
    with pytest.raises(IdentityError):
        NodeIdentity.from_file(str(broken))


def test_signed_peer_records():
    identity = NodeIdentity.generate()
    node = Node(NodeConfig(advertise_host="127.0.0.1"), identity=identity)

    record = node.self_peer_record()
    assert record["public_key"] == identity.public_key_hex
    assert node._verify_peer_record(record) is True

    tampered = {**record, "host": "evil.example.com"}
    assert node._verify_peer_record(tampered) is False

    stale = {**record, "timestamp": time.time() - 10_000}
    stale["signature"] = identity.sign_hex(Node._peer_record_payload(stale))
    assert node._verify_peer_record(stale) is False

    unsigned = {key: value for key, value in record.items() if key != "signature"}
    assert node._verify_peer_record(unsigned) is False


def test_controller_selection_is_deterministic():
    node = Node(NodeConfig(controller_count=2, epoch_length=1), identity=NodeIdentity.generate())
    identities = [NodeIdentity.generate() for _ in range(3)]
    peers = [_peer(identity, port) for identity, port in zip(identities, (9001, 9002, 9003))]
    node.PEERS = list(peers)

    first = node._select_controllers()
    node.PEERS = [peers[2], peers[0], peers[1]]
    second = node._select_controllers()
    assert [p["public_key"] for p in first] == [p["public_key"] for p in second]
    assert len(first) == 2

    keyless = Node(NodeConfig(), identity=NodeIdentity.generate())
    keyless.PEERS = [{"host": "127.0.0.1", "api_port": 1, "p2p_port": 1, "peer_port": 1,
                      "controller_port": 1}]
    assert keyless._select_controllers() == []


def _peer(identity: NodeIdentity, api_port: int) -> dict:
    record = {
        "chain_id": CHAIN_ID,
        "host": "127.0.0.1",
        "api_port": api_port,
        "p2p_port": api_port + 1,
        "peer_port": api_port + 2,
        "controller_port": api_port + 3,
        "timestamp": time.time(),
        "tls": False,
        "public_key": identity.public_key_hex,
    }
    record["signature"] = identity.sign_hex(Node._peer_record_payload(record))
    return record


# ---------------------------------------------------------------------- #
# Fork choice / reorg
# ---------------------------------------------------------------------- #

def test_fork_choice_reorgs_to_the_longest_chain():
    public_a, private_a = crypto.generate_keypair()
    public_b, private_b = crypto.generate_keypair()
    allocations = {
        public_a.hex(): 1_000_000 * SAN_BASE,
        public_b.hex(): 1_000_000 * SAN_BASE,
    }
    node = Node(
        NodeConfig(
            chain_id=CHAIN_ID,
            genesis_allocations=allocations,
            max_reorg_depth=10,
            require_state_root=False,
        ),
        identity=NodeIdentity(private_key=private_a, public_key=public_a),
    )
    genesis_hash = node.blockchain.tip.current_block_hash
    genesis_tx_count = len(node.blockchain.tip.transactions)

    def make_tx(private_key, public_key, nonce):
        payload = {
            "chain_id": CHAIN_ID,
            "sender": public_key.hex(),
            "nonce": nonce,
            "receiver": sample_address("ec"),
            "value": 1,
        }
        payload["signature"] = Transaction.sign_payload(payload, private_key)
        return Transaction(
            payload, fee_rate_for_transaction_count(genesis_tx_count)
        ).to_dict()

    def make_block(index, previous_hash, transactions, private_key, public_key):
        block = Block(
            index=index,
            previous_block_hash=previous_hash,
            validator=public_key.hex(),
            validator_signature=None,
            transactions=transactions,
            chain_id=CHAIN_ID,
        )
        block.validator_signature = crypto.sign(
            block.current_block_hash.encode(), private_key
        ).hex()
        return block

    block_a1 = make_block(1, genesis_hash, [make_tx(private_a, public_a, 0)], private_a, public_a)
    assert node._process_incoming_block(block_a1) is True
    assert node.blockchain.tip.current_block_hash == block_a1.current_block_hash

    block_b1 = make_block(1, genesis_hash, [make_tx(private_b, public_b, 0)], private_b, public_b)
    block_b2 = make_block(
        2, block_b1.current_block_hash, [make_tx(private_b, public_b, 1)], private_b, public_b
    )

    # block_b2 arrives before block_b1: buffered, no state change yet
    assert node._process_incoming_block(block_b2) is True
    assert node.blockchain.tip.current_block_hash == block_a1.current_block_hash

    # block_b1 arrives: the branch assembles and wins (longer chain)
    assert node._process_incoming_block(block_b1) is True
    assert node.blockchain.tip.current_block_hash == block_b2.current_block_hash
    assert node.blockchain.tip.index == 2

    # A's reorged-out transaction is back in the mempool and its balance untouched
    assert any(tx.sender == public_a.hex() for tx in node.transaction_pool)
    assert node.blockchain.SAN.get(address_of(public_a)) == 1_000_000 * SAN_BASE
    assert node.blockchain.nonces.get(address_of(public_b)) == 2

    # the winning branch keeps extending normally
    block_b3 = make_block(
        3, block_b2.current_block_hash, [make_tx(private_b, public_b, 2)], private_b, public_b
    )
    assert node._process_incoming_block(block_b3) is True
    assert node.blockchain.tip.index == 3


def test_reorg_depth_limit_is_enforced():
    public_a, private_a = crypto.generate_keypair()
    public_b, private_b = crypto.generate_keypair()
    allocations = {
        public_a.hex(): 1_000_000 * SAN_BASE,
        public_b.hex(): 1_000_000 * SAN_BASE,
    }
    node = Node(
        NodeConfig(
            chain_id=CHAIN_ID,
            genesis_allocations=allocations,
            max_reorg_depth=0,
            require_state_root=False,
        ),
        identity=NodeIdentity(private_key=private_a, public_key=public_a),
    )
    genesis_hash = node.blockchain.tip.current_block_hash
    rate = fee_rate_for_transaction_count(len(node.blockchain.tip.transactions))

    def signed_tx(private_key, public_key, nonce):
        payload = {
            "chain_id": CHAIN_ID,
            "sender": public_key.hex(),
            "nonce": nonce,
            "receiver": sample_address("ec"),
            "value": 1,
        }
        payload["signature"] = Transaction.sign_payload(payload, private_key)
        return Transaction(payload, rate).to_dict()

    def signed_block(index, previous_hash, transactions, private_key, public_key):
        block = Block(
            index=index,
            previous_block_hash=previous_hash,
            validator=public_key.hex(),
            validator_signature=None,
            transactions=transactions,
            chain_id=CHAIN_ID,
        )
        block.validator_signature = crypto.sign(
            block.current_block_hash.encode(), private_key
        ).hex()
        return block

    first = signed_block(1, genesis_hash, [signed_tx(private_b, public_b, 0)], private_b, public_b)
    assert node._process_incoming_block(first) is True

    competing = signed_block(1, genesis_hash, [signed_tx(private_a, public_a, 0)], private_a, public_a)
    assert node._process_incoming_block(competing) is False
    assert node.blockchain.tip.current_block_hash == first.current_block_hash


# ---------------------------------------------------------------------- #
# Entry point
# ---------------------------------------------------------------------- #

def test_run_uses_configured_api_port(monkeypatch):
    import uvicorn

    import run

    captured = {}

    def fake_run(app, **kwargs):
        captured.update(kwargs)

    monkeypatch.setenv("SAN_API_PORT", "18999")
    monkeypatch.setattr(uvicorn, "run", fake_run)
    run.main()

    assert captured["port"] == 18999
    assert captured["workers"] == 1


# ---------------------------------------------------------------------- #
# Faz B: gas
# ---------------------------------------------------------------------- #

def test_vm_charges_gas_and_enforces_limit():
    from SANVM.VM import OutOfGas

    vm = SANVirtualMachine(Storage(), gas_limit=100_000)
    vm.run(PenaParser().parse('x = 1\nprint("hello")'))
    used = vm.gas_used
    assert used > 0

    vm2 = SANVirtualMachine(Storage(), gas_limit=100_000)
    vm2.run(PenaParser().parse('x = 1\nprint("hello")'))
    assert vm2.gas_used == used  # deterministic

    with pytest.raises(OutOfGas):
        SANVirtualMachine(Storage(), gas_limit=3).run(
            PenaParser().parse('x = 1\nprint("hello")')
        )


def test_transaction_gas_rules_and_escrow_fee():
    public_key, private_key = crypto.generate_keypair()

    execution_payload = {
        "chain_id": CHAIN_ID,
        "sender": public_key.hex(),
        "nonce": 0,
        "gas_limit": 1000,
        "gas_price": 2,
        "bytecode": [["PUSH", 1], ["HALT"]],
    }
    execution_payload["signature"] = Transaction.sign_payload(execution_payload, private_key)

    stored = Transaction(execution_payload, MIN_FEE_PER_BYTE)
    size_fee = len(Transaction._fee_basis(execution_payload)) * MIN_FEE_PER_BYTE
    assert stored.fee == size_fee + 1000 * 2
    assert Transaction.expected_fee(execution_payload, MIN_FEE_PER_BYTE) == stored.fee

    # Execution without gas fields is rejected.
    without_gas = {k: v for k, v in execution_payload.items() if k not in ("gas_limit", "gas_price")}
    with pytest.raises(ValueError):
        Transaction(without_gas, MIN_FEE_PER_BYTE)

    # Plain transfers must not set gas fields.
    transfer = {
        "chain_id": CHAIN_ID,
        "sender": public_key.hex(),
        "nonce": 0,
        "receiver": sample_address("gg"),
        "value": 1,
        "gas_limit": 5,
        "gas_price": 1,
    }
    with pytest.raises(ValueError):
        Transaction(transfer, MIN_FEE_PER_BYTE)

    # Below the minimum gas price is rejected.
    cheap = {**execution_payload, "gas_price": 0}
    with pytest.raises(ValueError):
        Transaction(cheap, MIN_FEE_PER_BYTE)


def test_node_charges_gas_and_refunds_unused():
    sender_public, sender_private = crypto.generate_keypair()
    validator_identity = NodeIdentity.generate()
    allocations = {
        sender_public.hex(): 1_000_000 * SAN_BASE,
        validator_identity.public_key_hex: 1_000_000 * SAN_BASE,
    }
    node = Node(
        NodeConfig(
            chain_id=CHAIN_ID,
            genesis_allocations=allocations,
            block_threshold_fee=0.0001,
        ),
        identity=validator_identity,
    )

    thread = asyncio_run(
        node.submit_transaction(
            signed_payload_from(
                sender_public,
                sender_private,
                0,
                gas_limit=100_000,
                gas_price=1,
                contract_code={
                    "command": "deploy",
                    "contract_id": "gastest",
                    "pena_code": "value = 1\nfunction get() {\n return value\n}\n",
                },
            )
        )
    )
    assert thread["status"] == "committed"

    tx = node.blockchain.tip.transactions[0]
    assert "gastest" in node.storage.contracts

    sender_address = address_of(sender_public)
    validator_address = address_of(validator_identity.public_key)
    size_fee = len(Transaction._fee_basis(tx)) * node.blockchain.next_fee_rate()
    charged = 1_000_000 * SAN_BASE - node.blockchain.SAN[sender_address]

    # Unused gas was refunded, the base fee was burned and the validator got
    # the remainder (the size fee when gas_price == base_fee).
    burned = node.blockchain.total_burned
    assert burned > 0
    assert node.blockchain.SAN[validator_address] == 1_000_000 * SAN_BASE + charged - burned
    assert size_fee < charged < size_fee + tx["gas_limit"] * tx["gas_price"]


def signed_payload_from(public_key, private_key, nonce, **fields):
    payload = {
        "chain_id": CHAIN_ID,
        "sender": public_key.hex(),
        "nonce": nonce,
    }
    payload.update(fields)
    payload["signature"] = Transaction.sign_payload(payload, private_key)
    return payload


def test_out_of_gas_burns_escrow_and_rolls_back():
    sender_public, sender_private = crypto.generate_keypair()
    validator_identity = NodeIdentity.generate()
    allocations = {
        sender_public.hex(): 1_000_000 * SAN_BASE,
        validator_identity.public_key_hex: 1_000_000 * SAN_BASE,
    }
    node = Node(
        NodeConfig(
            chain_id=CHAIN_ID,
            genesis_allocations=allocations,
            block_threshold_fee=0.0001,
        ),
        identity=validator_identity,
    )

    deploy = asyncio_run(
        node.submit_transaction(
            signed_payload_from(
                sender_public,
                sender_private,
                0,
                gas_limit=100_000,
                gas_price=1,
                contract_code={
                    "command": "deploy",
                    "contract_id": "oog",
                    "pena_code": "value = 1\nfunction get() {\n return value\n}\nfunction set(v) {\n value = v\n return value\n}\n",
                },
            )
        )
    )
    assert deploy["status"] == "committed"
    storage_before = node.storage.contracts["oog"]["storage"]

    # Too little gas: the call fails, the escrow is burned and state is intact.
    call = asyncio_run(
        node.submit_transaction(
            signed_payload_from(
                sender_public,
                sender_private,
                1,
                gas_limit=2,
                gas_price=1,
                contract_code={
                    "command": "run",
                    "contract_id": "oog",
                    "function_name": "set",
                    "params": [42],
                },
            )
        )
    )
    assert call["status"] == "committed"
    assert node.storage.contracts["oog"]["storage"] == storage_before

    tx = node.blockchain.tip.transactions[0]
    assert tx["gas_limit"] == 2
    size_fee = len(Transaction._fee_basis(tx)) * node.blockchain.next_fee_rate()
    assert call["fee"] == size_fee + 2  # full escrow burned, no refund


# ---------------------------------------------------------------------- #
# Faz D: merkle proofs and state commitments
# ---------------------------------------------------------------------- #

def test_merkle_root_proof_and_verification():
    from blockchain.merkle import (
        EMPTY_ROOT,
        merkle_proof,
        merkle_root,
        verify_merkle_proof,
    )

    assert merkle_root([]) == EMPTY_ROOT
    leaves = ["a", "b", "c", "d", "e"]
    root = merkle_root(leaves)
    assert root == merkle_root(list(leaves))  # deterministic

    for index, leaf in enumerate(leaves):
        proof = merkle_proof(leaves, index)
        assert verify_merkle_proof(root, leaf, proof, index) is True
        assert verify_merkle_proof(root, f"tampered-{leaf}", proof, index) is False

    other_root = merkle_root(leaves[:-1])
    assert other_root != root


def test_block_commits_to_tx_root_and_state_root():
    from blockchain.merkle import merkle_root, verify_merkle_proof

    node = Node(NodeConfig(chain_id=CHAIN_ID), identity=NodeIdentity.generate())
    tx = {"chain_id": CHAIN_ID, "sender": "0x" + "ab" * 20, "nonce": 0, "fee": 1}
    tx2 = dict(tx, nonce=1)
    block = Block(
        index=1,
        previous_block_hash=node.blockchain.tip.current_block_hash,
        validator="v",
        validator_signature=None,
        transactions=[tx, tx2],
        chain_id=CHAIN_ID,
        state_root="deadbeef",
    )
    assert block.tx_root == merkle_root([tx, tx2])
    assert block.state_root == "deadbeef"

    header = block.to_header_dict()
    assert "transactions" not in header and header["tx_root"] == block.tx_root

    # Tampering with the announced tx_root is rejected on decode.
    with pytest.raises(ValueError):
        Block.from_dict({**block.to_dict(), "tx_root": "00" * 32})

    proof = __import__("blockchain.merkle", fromlist=["merkle_proof"]).merkle_proof([tx, tx2], 1)
    assert verify_merkle_proof(block.tx_root, tx2, proof, 1) is True


def test_state_root_binds_the_ledger_state():
    public_key, private_key = crypto.generate_keypair()
    allocations = {public_key.hex(): 1_000_000 * SAN_BASE}
    identity = NodeIdentity(private_key=private_key, public_key=public_key)
    node = Node(
        NodeConfig(
            chain_id=CHAIN_ID,
            genesis_allocations=allocations,
            block_threshold_fee=0.0001,
        ),
        identity=identity,
    )

    root_before = node.current_state_root()
    result = asyncio_run(
        node.submit_transaction(
            signed_payload_from(public_key, private_key, 0, receiver=sample_address("d1"), value=5)
        )
    )
    assert result["status"] == "committed"
    assert node.blockchain.tip.state_root == node.current_state_root()
    assert node.current_state_root() != root_before

    # A block claiming a different post-state root is rejected.
    block = node.blockchain.tip
    bogus = Block(
        index=block.index + 1,
        previous_block_hash=block.current_block_hash,
        validator=public_key.hex(),
        validator_signature=None,
        transactions=[],
        timestamp=block.timestamp + 1,
        chain_id=CHAIN_ID,
        state_root="00" * 32,
    )
    bogus.validator_signature = identity.sign_hex(bogus.current_block_hash.encode())
    assert node.verify_block(bogus) is False


# ---------------------------------------------------------------------- #
# Proposer fallback (rounds)
# ---------------------------------------------------------------------- #

def test_proposer_rounds_rotate_deterministically():
    node = Node(NodeConfig(chain_id=CHAIN_ID), identity=NodeIdentity.generate())
    active = {"0x" + char * 40: 100 for char in "abc"}
    seen = [node.expected_proposer(1, active, round_) for round_ in range(3)]
    assert len(set(seen)) == 3
    assert node.expected_proposer(1, active, 3) == seen[0]  # cycles
    assert node.expected_proposer(2, active, 0) == seen[0]  # height changes the base
    shuffled = dict(reversed(list(active.items())))
    assert node.expected_proposer(1, shuffled, 1) == seen[1]  # order independent


def test_proposer_fallback_produces_at_a_later_round():
    public_key, private_key = crypto.generate_keypair()
    other_public, other_private = crypto.generate_keypair()
    allocations = {
        public_key.hex(): 1_000_000 * SAN_BASE,
        other_public.hex(): 1_000_000 * SAN_BASE,
    }
    identity = NodeIdentity(private_key=private_key, public_key=public_key)
    node = Node(
        NodeConfig(
            chain_id=CHAIN_ID,
            genesis_allocations=allocations,
            min_validator_stake=100 * SAN_BASE,
            block_threshold_fee=0.0001,
            proposer_timeout=0.05,
            max_proposer_rounds=8,
            peer_check_interval=0.05,
        ),
        identity=identity,
    )

    async def flow():
        await node.submit_transaction(
            signed_payload_from(
                public_key, private_key, 0,
                validator={"command": "deposit", "amount": 100 * SAN_BASE},
            )
        )
        await node.submit_transaction(
            signed_payload_from(
                other_public, other_private, 0,
                validator={"command": "deposit", "amount": 100 * SAN_BASE},
            )
        )
        assert node.blockchain.tip.index == 2
        active = node.blockchain.active_validators()
        assert len(active) == 2

        our_address = address_of(public_key)
        first_round = next(
            round_
            for round_ in range(4)
            if node.expected_proposer(3, active, round_) == our_address
        )
        wrong_round = next(
            round_
            for round_ in range(4)
            if node.expected_proposer(3, active, round_) != our_address
        )

        # A block claimed at a round where this validator is not the proposer
        # is rejected.
        wrong = Block(
            index=3,
            previous_block_hash=node.blockchain.tip.current_block_hash,
            validator=public_key.hex(),
            validator_signature=None,
            transactions=[],
            chain_id=CHAIN_ID,
            round=wrong_round,
        )
        wrong.validator_signature = identity.sign_hex(wrong.current_block_hash.encode())
        assert node.verify_block(wrong) is False

        transfer = signed_payload_from(
            public_key, private_key, 1, receiver=sample_address("71"), value=1
        )
        result = await node.submit_transaction(transfer)
        if result["status"] != "committed":
            # The round-0 proposer is offline: this node must take over at its
            # own round once the proposer timeout elapses.
            for _ in range(80):
                if node.blockchain.tip.index >= 3:
                    break
                await node._maybe_produce_from_pool()
                await asyncio.sleep(0.05)

        assert node.blockchain.tip.index == 3
        block = node.blockchain.tip
        assert block.validator == public_key.hex()
        assert first_round <= block.round < first_round + 8
        assert node.expected_proposer(3, active, block.round) == our_address

    asyncio_run(flow())


# ---------------------------------------------------------------------- #
# Audit fixes
# ---------------------------------------------------------------------- #

def _deposit_pair(node, first, second):
    """Register two validators (helper for audit tests)."""
    (public_a, private_a), (public_b, private_b) = first, second
    return [
        signed_payload_from(
            public_a, private_a, 0,
            validator={"command": "deposit", "amount": 100 * SAN_BASE},
        ),
        signed_payload_from(
            public_b, private_b, 0,
            validator={"command": "deposit", "amount": 100 * SAN_BASE},
        ),
    ]


def test_round_timing_rejects_unjustified_rounds():
    key_a = crypto.generate_keypair()
    key_b = crypto.generate_keypair()
    allocations = {key[0].hex(): 1_000_000 * SAN_BASE for key in (key_a, key_b)}
    node = Node(
        NodeConfig(
            chain_id=CHAIN_ID,
            genesis_allocations=allocations,
            min_validator_stake=100 * SAN_BASE,
            block_threshold_fee=0.0001,
            proposer_timeout=30.0,
            max_proposer_rounds=8,
            require_state_root=False,
        ),
        identity=NodeIdentity(private_key=key_a[1], public_key=key_a[0]),
    )

    async def flow():
        for payload in _deposit_pair(node, key_a, key_b):
            await node.submit_transaction(payload)
        active = node.blockchain.active_validators()
        assert len(active) == 2 and node.blockchain.tip.index == 2

        # A round claim must be justified by the block timestamp: every node
        # derives the same verdict from the parent timestamp alone.
        proposer_round_one = node.expected_proposer(3, active, 1)
        key = next(
            key for key in (key_a, key_b)
            if address_of(key[0]) == proposer_round_one
        )
        tip = node.blockchain.tip

        def round_one_block(timestamp):
            block = Block(
                index=3,
                previous_block_hash=tip.current_block_hash,
                validator=key[0].hex(),
                validator_signature=None,
                transactions=[],
                timestamp=timestamp,
                chain_id=CHAIN_ID,
                round=1,
            )
            block.validator_signature = crypto.sign(
                block.current_block_hash.encode(), key[1]
            ).hex()
            return block

        # Timestamp borrowed from the parent: round 1 is not justified yet.
        assert node.verify_block(round_one_block(tip.timestamp)) is False
        # Fully justified timestamp: accepted without any local clock state.
        assert node.verify_block(
            round_one_block(tip.timestamp + node.config.proposer_timeout * 0.8 + 0.1)
        ) is True

    asyncio_run(flow())


def test_dead_peer_claims_are_ignored():
    import json as _json

    node = Node(NodeConfig(chain_id=CHAIN_ID), identity=NodeIdentity.generate())
    victim = _peer(NodeIdentity.generate(), 9201)
    node.PEERS.append(victim)

    class _Socket:
        async def send(self, _message):  # pragma: no cover - not asserted
            pass

    asyncio_run(
        node._handle_peer_message(
            _Socket(),
            _json.dumps({"type": "DEAD_PEER", "peer": {"host": victim["host"], "api_port": victim["api_port"]}}),
        )
    )
    assert any(p["api_port"] == victim["api_port"] for p in node.PEERS)


def test_finality_weights_are_frozen_at_commit():
    key_a = crypto.generate_keypair()
    key_b = crypto.generate_keypair()
    key_c = crypto.generate_keypair()
    allocations = {key[0].hex(): 1_000_000 * SAN_BASE for key in (key_a, key_b, key_c)}
    node = Node(
        NodeConfig(
            chain_id=CHAIN_ID,
            genesis_allocations=allocations,
            min_validator_stake=100 * SAN_BASE,
            block_threshold_fee=0.0001,
            proposer_timeout=0.05,
            peer_check_interval=0.02,
        ),
        identity=NodeIdentity(private_key=key_a[1], public_key=key_a[0]),
    )

    async def flow():
        async def ensure_height(target: int) -> bool:
            for _ in range(200):
                if node.blockchain.tip.index >= target:
                    return True
                await node._maybe_produce_from_pool()
                await asyncio.sleep(0.02)
            return False

        for index, key in enumerate((key_a, key_b, key_c), start=1):
            await node.submit_transaction(
                signed_payload_from(
                    key[0], key[1], 0,
                    validator={"command": "deposit", "amount": 100 * SAN_BASE},
                )
            )
            assert await ensure_height(index)
        await node.submit_transaction(
            signed_payload_from(
                key_a[0], key_a[1], 1, receiver=sample_address("81"), value=1
            )
        )
        assert await ensure_height(4)
        height = node.blockchain.tip.index
        block_hash = node.blockchain.tip.current_block_hash
        assert len(node._finality_sets[height]) == 3

        # Simulate the audit scenario: two validators leave the registry, so a
        # naive "current registry" tally would see a single validator at 100%.
        addresses = [address_of(key[0]) for key in (key_a, key_b, key_c)]
        for address in addresses[1:]:
            node.blockchain.validators.pop(address, None)

        node._finality_votes[height] = {block_hash: {addresses[0]: {}}}
        node._tally_finality(height, block_hash)
        assert node.finalized_height < height, "frozen weights must prevent false finality"

        node._finality_votes[height][block_hash] = {address: {} for address in addresses}
        node._tally_finality(height, block_hash)
        assert node.finalized_height == height

    asyncio_run(flow())


def test_state_root_covers_vm_storage():
    from blockchain.merkle import merkle_root, state_entries

    base_storage = {"data": {}, "functions": {}, "contracts": {}}
    root_plain = merkle_root(state_entries({}, {}, {}, 0, base_storage))
    root_data = merkle_root(
        state_entries({}, {}, {}, 0, {**base_storage, "data": {"k": 1}})
    )
    root_funcs = merkle_root(
        state_entries({}, {}, {}, 0, {**base_storage, "functions": {"f": {"pc": 3}}})
    )
    assert root_plain != root_data
    assert root_plain != root_funcs
    assert root_data != root_funcs

    # End to end: a raw bytecode SET changes the committed state root.
    public_key, private_key = crypto.generate_keypair()
    allocations = {public_key.hex(): 1_000_000 * SAN_BASE}
    identity = NodeIdentity(private_key=private_key, public_key=public_key)
    node = Node(
        NodeConfig(
            chain_id=CHAIN_ID,
            genesis_allocations=allocations,
            block_threshold_fee=0.0001,
        ),
        identity=identity,
    )

    async def flow():
        result = await node.submit_transaction(
            signed_payload_from(
                public_key,
                private_key,
                0,
                gas_limit=100_000,
                gas_price=1,
                bytecode=[["PUSH", "k"], ["PUSH", 7], ["SET"], ["HALT"]],
            )
        )
        assert result["status"] == "committed"
        assert node.storage.data == {"k": 7}
        assert node.blockchain.tip.state_root == node.current_state_root()

    asyncio_run(flow())


def test_stale_tx_index_is_not_served(tmp_path):
    public_key, private_key = crypto.generate_keypair()
    allocations = {public_key.hex(): 1_000_000 * SAN_BASE}
    identity = NodeIdentity(private_key=private_key, public_key=public_key)
    node = Node(
        NodeConfig(
            chain_id=CHAIN_ID,
            genesis_allocations=allocations,
            block_threshold_fee=0.0001,
            db_path=str(tmp_path / "stale.kv"),
        ),
        identity=identity,
    )

    async def flow():
        payload = signed_payload_from(
            public_key, private_key, 0, receiver=sample_address("82"), value=1
        )
        result = await node.submit_transaction(payload)
        tx_id = result["tx_id"]
        assert node.get_transaction(tx_id) is not None

        # Simulate the reorg aftermath: the canonical block at that height is
        # replaced, but the old tx-index entry is still on disk.
        other = Block(
            index=1,
            previous_block_hash=node.blockchain.chain[0].current_block_hash,
            validator="v",
            validator_signature="s",
            transactions=[],
            chain_id=CHAIN_ID,
        )
        node.blockchain.chain[1] = other
        assert node.get_transaction(tx_id) is None

    asyncio_run(flow())
    node.store.close()


def test_storage_growth_is_priced_and_capped():
    from blockchain.storage import MemoryStore  # noqa: F401  (import parity)
    from SANVM.gas import payload_cost
    from SANVM.OpCode import OpCode
    from SANVM.VM import VMError

    vm = SANVirtualMachine(Storage(), gas_limit=10 ** 6, verbose=False)
    blob = "x" * 10_000
    vm.run(
        [
            OpCode.PUSH.value, "l", OpCode.PUSH.value, [], OpCode.SET.value,
            OpCode.PUSH.value, "l", OpCode.PUSH.value, blob, OpCode.LIST_APPEND.value,
        ]
    )
    assert vm.gas_used >= payload_cost(blob)

    with pytest.raises(VMError):
        SANVirtualMachine(Storage(), gas_limit=10 ** 6, verbose=False).run(
            [OpCode.PUSH.value, "y" * 70_000]
        )


def test_call_opcode_respects_the_call_depth():
    from SANVM.OpCode import OpCode
    from SANVM.VM import VMError

    vm = SANVirtualMachine(Storage(), gas_limit=10 ** 6, verbose=False, max_call_depth=4)
    with pytest.raises(VMError):
        # PUSH 0; CALL jumps to pc 0 and grows the frame stack forever.
        vm.run([OpCode.PUSH.value, 0, OpCode.CALL.value])


# ---------------------------------------------------------------------- #
# Audit fixes (round 2)
# ---------------------------------------------------------------------- #

def _retime_tip(node) -> None:
    """Give the genesis block a realistic timestamp and rehash it.

    Block helpers build timestamps relative to the tip; a genesis at 0.0 would
    otherwise make every timestamp look backdated.
    """
    tip = node.blockchain.tip
    tip.timestamp = time.time()
    tip.current_block_hash = tip.calculate_hash()


def _solo_node(**overrides):
    public_key, private_key = crypto.generate_keypair()
    config = NodeConfig(
        chain_id=CHAIN_ID,
        genesis_allocations={public_key.hex(): 1_000_000 * SAN_BASE},
        **overrides,
    )
    identity = NodeIdentity(private_key=private_key, public_key=public_key)
    return Node(config, identity=identity), public_key


def _next_block(node, public_key):
    return Block(
        index=node.blockchain.tip.index + 1,
        previous_block_hash=node.blockchain.tip.current_block_hash,
        validator=public_key.hex(),
        validator_signature=None,
        transactions=[],
        chain_id=CHAIN_ID,
    )


def test_commit_block_requires_a_valid_signature():
    node, public_key = _solo_node()
    block = seal_block(node, _next_block(node, public_key))
    block.validator_signature = None

    assert node._commit_block(block) is False
    assert node.blockchain.tip.index == 0


def test_state_root_is_required_on_every_non_genesis_block():
    node, public_key = _solo_node()
    block = seal_block(node, _next_block(node, public_key))
    block.state_root = None
    block.current_block_hash = block.calculate_hash()
    block.validator_signature = node._sign_block_hash(block.current_block_hash)

    assert node.verify_block(block) is False


def test_reorg_rollback_restores_finality_bookkeeping():
    node, _ = _solo_node()
    node._finality_sets[3] = {"0xaa": 10}
    node._finality_votes[3] = {"hash": {"0xaa": {"signature": "s"}}}
    snapshot = node._capture_state()

    node._finality_sets[3] = {"0xbb": 1}
    node._finality_votes.clear()
    node._restore_state(snapshot)

    assert node._finality_sets[3] == {"0xaa": 10}
    assert 3 in node._finality_votes


def test_vm_rejects_oversized_integers_and_prices_multiplies():
    from SANVM.OpCode import OpCode
    from SANVM.VM import VMError

    vm = SANVirtualMachine(Storage(), gas_limit=10 ** 9, verbose=False)
    with pytest.raises(VMError):
        vm.run([OpCode.PUSH.value, 1 << (SANVirtualMachine.MAX_INT_BITS + 1)])

    big = (1 << 2040) - 1
    small = SANVirtualMachine(Storage(), gas_limit=10 ** 9, verbose=False)
    small.run([OpCode.PUSH.value, 3, OpCode.PUSH.value, 5, OpCode.MUL.value])
    big_vm = SANVirtualMachine(Storage(), gas_limit=10 ** 9, verbose=False)
    big_vm.run([OpCode.PUSH.value, big, OpCode.PUSH.value, big, OpCode.MUL.value])
    assert big_vm.gas_used > small.gas_used


def test_contract_payloads_are_validated_before_execution():
    from SANVM.ContractManager import ContractManager

    assert Transaction.validate_contract_code({}) is None
    assert Transaction.validate_contract_code({"contract_code": 5}) is not None
    assert (
        Transaction.validate_contract_code(
            {"contract_code": {"command": "deploy", "contract_id": 7, "bytecode": [1]}}
        )
        is not None
    )
    assert (
        Transaction.validate_contract_code(
            {
                "contract_code": {
                    "command": "deploy",
                    "contract_id": "c1",
                    "bytecode": [1],
                }
            }
        )
        is None
    )

    manager = ContractManager(Storage())
    with pytest.raises(ValueError):
        manager.deploy_contract(7, [0])


def test_round_deadline_is_consensus_state():
    from blockchain.Blockchain import GOVERNANCE_PARAMETERS, governance_value_ok

    node, public_key = _solo_node(require_state_root=False)
    _retime_tip(node)
    assert node.blockchain.parameters["proposer_timeout_ms"] == 6000
    assert node.blockchain.proposer_timeout == 6.0
    assert governance_value_ok("proposer_timeout_ms", 30_000) is True
    assert "proposer_timeout_ms" in GOVERNANCE_PARAMETERS

    # Governance changes the round deadline: every node reads it from state,
    # not from its local environment.
    node.blockchain.parameters["proposer_timeout_ms"] = 30_000
    tip = node.blockchain.tip

    def round_one_block(timestamp):
        return seal_block(
            node,
            Block(
                index=1,
                previous_block_hash=tip.current_block_hash,
                validator=public_key.hex(),
                validator_signature=None,
                transactions=[],
                timestamp=timestamp,
                chain_id=CHAIN_ID,
                round=1,
            ),
        )

    assert node.verify_block(round_one_block(tip.timestamp + 12)) is False
    assert node.verify_block(round_one_block(tip.timestamp + 24.1)) is True


def test_pruning_is_skipped_without_a_snapshot_anchor(tmp_path):
    public_key, private_key = crypto.generate_keypair()
    node = Node(
        NodeConfig(
            chain_id=CHAIN_ID,
            genesis_allocations={public_key.hex(): 1_000_000 * SAN_BASE},
            require_state_root=False,
            db_path=str(tmp_path / "prune.kv"),
            prune_keep=1,
            snapshot_interval=0,
        ),
        identity=NodeIdentity(private_key=private_key, public_key=public_key),
    )

    async def flow():
        for _ in range(3):
            block = node._build_block()
            assert node._commit_block(block) is True
        latest = node.blockchain.tip
        voter = sample_address("aa")
        node._finality_sets[latest.index] = {voter: 100}
        node._finality_votes[latest.index] = {
            latest.current_block_hash: {voter: {"signature": "s"}}
        }
        node._tally_finality(latest.index, latest.current_block_hash)
        assert node.finalized_height == latest.index

    asyncio_run(flow())
    try:
        # No snapshot exists, so pruning must be skipped: otherwise a restart
        # would refuse a pruned store without a matching anchor.
        assert node.store.get_meta("pruned_below") is None
        assert node.store.count_blocks() == 4
    finally:
        node.store.close()


def test_vm_meters_wide_integer_adds():
    from SANVM.OpCode import OpCode

    big = (1 << 2040) - 1
    small = SANVirtualMachine(Storage(), gas_limit=10 ** 9, verbose=False)
    small.run([OpCode.PUSH.value, 3, OpCode.PUSH.value, 5, OpCode.ADD.value])
    wide = SANVirtualMachine(Storage(), gas_limit=10 ** 9, verbose=False)
    wide.run([OpCode.PUSH.value, big, OpCode.PUSH.value, big, OpCode.ADD.value])
    assert wide.gas_used > small.gas_used


def test_block_reward_is_minted_to_the_configured_address():
    from blockchain.address import normalize_address

    public_key, private_key = crypto.generate_keypair()
    reward = sample_address("cc")
    node = Node(
        NodeConfig(
            chain_id=CHAIN_ID,
            genesis_allocations={public_key.hex(): 1_000_000 * SAN_BASE},
            block_reward=2.0,
            reward_address=reward,
        ),
        identity=NodeIdentity(private_key=private_key, public_key=public_key),
    )
    assert node.reward_address == normalize_address(reward)
    assert node.blockchain.block_reward == 2 * SAN_BASE
    assert node.blockchain.parameters["block_reward"] == 2 * SAN_BASE

    async def flow():
        assert node._commit_block(node._build_block()) is True

    asyncio_run(flow())
    assert node.blockchain.SAN.get(reward) == 2 * SAN_BASE


def test_genesis_endpoint_exposes_public_bootstrap_data():
    from app.routes import get_genesis

    public_key, _ = crypto.generate_keypair()
    allocation = {address_of(public_key): 5 * SAN_BASE}
    node = Node(
        NodeConfig(chain_id=CHAIN_ID, genesis_allocations=allocation),
        identity=NodeIdentity.generate(),
    )
    payload = asyncio_run(get_genesis(node))
    assert payload["chain_id"] == CHAIN_ID
    assert payload["genesis_allocation"] == {address_of(public_key): str(5 * SAN_BASE)}
    assert payload["genesis_hash"] == node.blockchain.chain[0].current_block_hash
    assert "block_reward" in payload["parameters"]


def test_min_block_interval_is_a_consensus_rule_for_reward_chains():
    public_key, private_key = crypto.generate_keypair()
    node = Node(
        NodeConfig(
            chain_id=CHAIN_ID,
            genesis_allocations={public_key.hex(): 1_000_000 * SAN_BASE},
            block_reward=1.0,
            require_state_root=False,
        ),
        identity=NodeIdentity(private_key=private_key, public_key=public_key),
    )
    assert node.blockchain.min_block_interval == 1.0
    _retime_tip(node)

    tip = node.blockchain.tip

    def block_at(timestamp):
        block = Block(
            index=1,
            previous_block_hash=tip.current_block_hash,
            validator=public_key.hex(),
            validator_signature=None,
            transactions=[],
            timestamp=timestamp,
            chain_id=CHAIN_ID,
        )
        block.validator_signature = node._sign_block_hash(block.current_block_hash)
        return block

    assert node.verify_block(block_at(tip.timestamp + 0.1)) is False
    assert node.verify_block(block_at(tip.timestamp + 1.5)) is True


def test_finality_votes_far_ahead_of_the_tip_are_dropped():
    public_key, private_key = crypto.generate_keypair()
    node = Node(
        NodeConfig(chain_id=CHAIN_ID, genesis_allocations={public_key.hex(): SAN_BASE}),
        identity=NodeIdentity(private_key=private_key, public_key=public_key),
    )

    async def flow():
        vote = {
            "chain_id": CHAIN_ID,
            "public_key": node.get_public_key(),
            "height": node.blockchain.tip.index + 1_000,
            "block_hash": "ab" * 32,
            "timestamp": time.time(),
        }
        vote["signature"] = node.identity.sign_hex(node._vote_payload(vote))
        await node._handle_finality_vote(vote)

    asyncio_run(flow())
    assert node._finality_votes == {}


def test_sync_picks_the_longest_compatible_peer():
    from helpers import FakePeerServer

    public_key, private_key = crypto.generate_keypair()
    node = Node(
        NodeConfig(chain_id=CHAIN_ID, genesis_allocations={public_key.hex(): SAN_BASE}),
        identity=NodeIdentity(private_key=private_key, public_key=public_key),
    )

    def status(height, chain=CHAIN_ID, allocation=None):
        return {
            "chain_id": chain,
            "genesis_allocation": allocation or node._genesis_allocation_fingerprint(),
            "height": height,
            "finalized_height": height,
        }

    async def responder(recv, send):
        pass

    async def flow():
        short = FakePeerServer(NodeIdentity.generate(), responder, free_port(), status(3))
        long = FakePeerServer(NodeIdentity.generate(), responder, free_port(), status(9))
        foreign = FakePeerServer(
            NodeIdentity.generate(), responder, free_port(), status(99, chain="other")
        )
        await short.start()
        await long.start()
        await foreign.start()
        try:
            for server in (short, long, foreign):
                node.PEERS.append(
                    {
                        "host": "127.0.0.1",
                        "p2p_port": server.port,
                        "peer_port": server.port,
                        "controller_port": server.port,
                        "api_port": server.port,
                        "tls": False,
                    }
                )
            chosen = await node._select_sync_peer()
            assert chosen is not None and chosen["p2p_port"] == long.port
        finally:
            for server in (short, long, foreign):
                await server.stop()

    asyncio_run(flow())
