"""Faz C verification: validators, staking, proposer rule, slashing, finality.

Run from the repository root:

    python tests/smoke_p3.py
"""
import asyncio
import json
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

from helpers import (  # noqa: E402
    CHAIN_ID,
    SAN_BASE,
    address_of,
    check,
    make_config,
    sample_address,
    setup_identity,
    signed_payload,
    summary,
)

from blockchain import crypto  # noqa: E402
from blockchain.Block import Block  # noqa: E402
from blockchain.economics import DEFAULT_SLASH_BPS  # noqa: E402
from blockchain.identity import NodeIdentity  # noqa: E402
from blockchain.Transaction import Transaction  # noqa: E402
from network.Node import Node  # noqa: E402
from utils import canonical  # noqa: E402


def validator_tx(public_key, private_key, nonce, command, **extra):
    payload = {
        "chain_id": CHAIN_ID,
        "sender": public_key.hex() if isinstance(public_key, bytes) else public_key,
        "nonce": nonce,
        "validator": {"command": command, **extra},
    }
    payload["signature"] = Transaction.sign_payload(payload, private_key)
    return payload


# ---------------------------------------------------------------------- #
# C1/C2/C3 stake lifecycle
# ---------------------------------------------------------------------- #

async def lifecycle_checks(public_key, private_key):
    print("\n=== C1-C3 validator lifecycle ===")
    config = make_config(unbonding_period=3, min_validator_stake=1000 * SAN_BASE)
    node = Node(config)
    await node.start()
    address = address_of(public_key)
    start_balance = node.blockchain.SAN[address]

    result = await node.submit_transaction(
        validator_tx(public_key, private_key, 0, "deposit", amount=1000 * SAN_BASE)
    )
    check("deposit committed", result["status"] == "committed", json.dumps(result))
    check(
        "validator active with locked stake",
        node.blockchain.active_validators() == {address: 1000 * SAN_BASE},
        str(node.blockchain.active_validators()),
    )
    check(
        "stake left the spendable balance",
        node.blockchain.SAN[address] == start_balance - 1000 * SAN_BASE,
        f"balance={node.blockchain.SAN[address]}",
    )

    # Undelegate: still staked, but out of the active set.
    result = await node.submit_transaction(validator_tx(public_key, private_key, 1, "undelegate"))
    check("undelegate committed", result["status"] == "committed")
    check("undelegating validator is inactive", node.blockchain.active_validators() == {})
    check(
        "release height scheduled",
        node.blockchain.validators[address]["release_height"] == node.blockchain.tip.index + 3,
        str(node.blockchain.validators[address]),
    )

    # Withdraw too early is rejected by block validation.
    early = await node.submit_transaction(validator_tx(public_key, private_key, 2, "withdraw"))
    check("early withdraw rejected", early["status"] == "rejected", json.dumps(early))

    # Push the chain to the release height, then withdraw succeeds.
    for nonce in range(2, 6):
        await node.submit_transaction(
            signed_payload(public_key, private_key, nonce, receiver=sample_address("11"), value=1)
        )
    check("chain advanced past the unbonding period", node.blockchain.tip.index >= 5)
    result = await node.submit_transaction(validator_tx(public_key, private_key, 6, "withdraw"))
    check("withdraw after release committed", result["status"] == "committed", json.dumps(result))
    check("validator removed", address not in node.blockchain.validators)

    await node.stop()


# ---------------------------------------------------------------------- #
# C4 proposer rule
# ---------------------------------------------------------------------- #

async def proposer_checks(public_key, private_key):
    print("\n=== C4 deterministic proposer ===")
    other_public, other_private = crypto.generate_keypair()
    allocations = {
        public_key.hex(): 1_000_000 * SAN_BASE,
        other_public.hex(): 1_000_000 * SAN_BASE,
    }
    node = Node(
        make_config(
            min_validator_stake=100 * SAN_BASE,
            unbonding_period=5,
            genesis_allocations=allocations,
        )
    )
    await node.start()

    # Register two validators (this node and the other key are both funded).
    await node.submit_transaction(
        validator_tx(public_key, private_key, 0, "deposit", amount=500 * SAN_BASE)
    )
    await node.submit_transaction(
        validator_tx(other_public, other_private, 0, "deposit", amount=500 * SAN_BASE)
    )
    active = node.blockchain.active_validators()
    check("two validators active", len(active) == 2, str(active))

    first = node.expected_proposer(7, active)
    shuffled = dict(reversed(list(active.items())))
    second = node.expected_proposer(7, shuffled)
    check("proposer is order independent", first == second, f"{first} vs {second}")
    check("proposer is in the active set", first in active)

    # A block proposed by a non-proposer is rejected.
    tip = node.blockchain.tip
    impostor_public, impostor_private = crypto.generate_keypair()
    impostor_block = Block(
        index=tip.index + 1,
        previous_block_hash=tip.current_block_hash,
        validator=impostor_public.hex(),
        validator_signature=None,
        transactions=[],
        chain_id=CHAIN_ID,
    )
    impostor_block.validator_signature = crypto.sign(
        impostor_block.current_block_hash.encode(), impostor_private
    ).hex()
    check("non-proposer block rejected", node.verify_block(impostor_block) is False)

    await node.stop()


# ---------------------------------------------------------------------- #
# C5 slashing with evidence
# ---------------------------------------------------------------------- #

async def slashing_checks(public_key, private_key):
    print("\n=== C5 equivocation evidence and slashing ===")
    offender_public, offender_private = crypto.generate_keypair()
    allocations = {
        public_key.hex(): 1_000_000 * SAN_BASE,
        offender_public.hex(): 1_000_000 * SAN_BASE,
    }
    node = Node(
        make_config(
            min_validator_stake=100 * SAN_BASE,
            unbonding_period=5,
            genesis_allocations=allocations,
        )
    )
    await node.start()

    # The offender stakes below the minimum: registered (slashable) but
    # inactive, so this node stays the deterministic proposer for the evidence
    # transaction (there is no proposer fallback yet).
    stake = 50 * SAN_BASE
    await node.submit_transaction(
        validator_tx(public_key, private_key, 0, "deposit", amount=400 * SAN_BASE)
    )
    await node.submit_transaction(
        validator_tx(offender_public, offender_private, 0, "deposit", amount=stake)
    )
    offender_address = address_of(offender_public)
    check("offender is a registered validator", offender_address in node.blockchain.validators)
    check("offender is not in the active set", offender_address not in node.blockchain.active_validators())
    balance_after_deposit = node.blockchain.SAN[offender_address]

    def signed_vote(height, block_hash):
        vote = {
            "chain_id": CHAIN_ID,
            "public_key": offender_public.hex(),
            "height": height,
            "block_hash": block_hash,
            "timestamp": time.time(),
        }
        vote["signature"] = crypto.sign(canonical.dumps_bytes(vote), offender_private).hex()
        return vote

    result = await node.submit_transaction(
        validator_tx(
            public_key,
            private_key,
            1,
            "evidence",
            vote_a=signed_vote(3, "aa" * 32),
            vote_b=signed_vote(3, "bb" * 32),
        )
    )
    check("evidence committed", result["status"] == "committed", json.dumps(result))

    burned = stake * DEFAULT_SLASH_BPS // 10_000
    remaining = stake - burned
    check("offender removed from the validator set", offender_address not in node.blockchain.validators)
    check(
        "remaining stake returned, slashed share burned",
        node.blockchain.SAN[offender_address] == balance_after_deposit + remaining,
        f"balance={node.blockchain.SAN[offender_address]} expected={balance_after_deposit + remaining}",
    )
    check("total_slashed tracked", node.blockchain.total_slashed == burned)

    await node.stop()


# ---------------------------------------------------------------------- #
# C5 finality over a real two-node network
# ---------------------------------------------------------------------- #

async def finality_flow():
    print("\n=== C5 two-node finality (2/3 stake) ===")
    identity_a, identity_b = NodeIdentity.generate(), NodeIdentity.generate()
    allocations = {
        identity_a.public_key_hex: 1_000_000 * SAN_BASE,
        identity_b.public_key_hex: 1_000_000 * SAN_BASE,
    }
    config_a = make_config(
        min_validator_stake=100 * SAN_BASE,
        unbonding_period=5,
        genesis_allocations=allocations,
    )
    config_b = make_config(
        min_validator_stake=100 * SAN_BASE,
        unbonding_period=5,
        genesis_allocations=allocations,
        bootstrap=f"127.0.0.1:{config_a.api_port}",
    )
    node_a = Node(config_a, identity=identity_a)
    node_b = Node(config_b, identity=identity_b)
    await node_a.start()
    await node_b.start()

    node_b.add_peers([node_a.self_peer_record()])
    await node_b.register_to_network()
    async def peers_ready():
        return len(node_a.PEERS) == 1 and len(node_b.PEERS) == 1

    await wait_until("nodes discovered each other", peers_ready)

    def deposit(identity, nonce):
        payload = {
            "chain_id": CHAIN_ID,
            "sender": identity.public_key_hex,
            "nonce": nonce,
            "validator": {"command": "deposit", "amount": 500 * SAN_BASE},
        }
        payload["signature"] = Transaction.sign_payload(payload, identity.private_key)
        return payload

    # Block 1: no active validators yet, so either node may propose.
    result = await node_a.submit_transaction(deposit(identity_a, 0))
    check("A's own deposit committed", result["status"] == "committed", json.dumps(result))
    await wait_until("B applied block 1", lambda: node_b.blockchain.tip.index == 1)

    # Block 2: A is the only validator, so A must propose B's deposit.
    result = await node_a.submit_transaction(deposit(identity_b, 0))
    check("B's deposit committed", result["status"] == "committed", json.dumps(result))
    await wait_until("B applied block 2", lambda: node_b.blockchain.tip.index == 2)

    check(
        "both validators active on both nodes",
        len(node_a.blockchain.active_validators()) == 2
        and node_b.blockchain.active_validators() == node_a.blockchain.active_validators(),
        str(node_a.blockchain.active_validators()),
    )

    # Block 3: only the deterministic proposer may produce it.
    expected = node_a.expected_proposer(3)
    proposer = node_a if expected == address_of(identity_a.public_key) else node_b
    check("expected proposer computed", expected in node_a.blockchain.active_validators())

    transfer = signed_payload(
        identity_a.public_key, identity_a.private_key, 1, receiver=sample_address("f1"), value=2
    )
    result = await proposer.submit_transaction(transfer)
    check("block 3 committed by the expected proposer", result["status"] == "committed", json.dumps(result))

    async def finalized_on_both():
        return (
            node_a.finalized_height == 3
            and node_b.finalized_height == 3
            and node_a.finalized_hash == node_b.finalized_hash
            and node_b.finalized_hash == node_a.blockchain.chain[3].current_block_hash
        )

    await wait_until("block 3 finalized with 2/3 stake on both nodes", finalized_on_both, timeout=15)
    check(
        "finality checkpoint matches the tip",
        node_a.finalized_height == 3
        and node_a.finalized_hash == node_a.blockchain.chain[3].current_block_hash
        and node_b.finalized_hash == node_a.finalized_hash,
        f"A=({node_a.finalized_height},{node_a.finalized_hash[:12]}) B=({node_b.finalized_height},{node_b.finalized_hash[:12]})",
    )

    # Any RPC node accepts a transaction: non-proposers pool and forward it.
    next_height = node_a.blockchain.tip.index + 1
    expected_next = node_a.expected_proposer(next_height)
    non_proposer = node_b if expected_next == address_of(identity_a.public_key) else node_a
    forwarded = signed_payload(
        identity_a.public_key, identity_a.private_key, 2, receiver=sample_address("f2"), value=1
    )
    result = await non_proposer.submit_transaction(forwarded)
    check(
        "non-proposer RPC node pools and forwards the transaction",
        result.get("status") == "pooled" and "forwarded" in str(result.get("note", "")),
        json.dumps(result),
    )

    async def proposer_included_it():
        return (
            node_a.blockchain.tip.index == 4
            and node_b.blockchain.tip.index == 4
            and node_a.blockchain.tip.current_block_hash
            == node_b.blockchain.tip.current_block_hash
        )

    await wait_until("proposer included the forwarded transaction", proposer_included_it)

    async def height_four_finalized():
        return node_a.finalized_height == 4 and node_b.finalized_height == 4

    await wait_until("height 4 finalized on both nodes", height_four_finalized, timeout=20)

    # A longer branch that rewrites finalized history must be rejected.
    def fake_block(index, previous_hash):
        block = Block(
            index=index,
            previous_block_hash=previous_hash,
            validator=identity_a.public_key_hex,
            validator_signature=None,
            transactions=[],
            chain_id=CHAIN_ID,
        )
        block.validator_signature = identity_a.sign_hex(block.current_block_hash.encode())
        return block

    fake_2 = fake_block(2, node_a.blockchain.chain[1].current_block_hash)
    fake_3 = fake_block(3, fake_2.current_block_hash)
    fake_4 = fake_block(4, fake_3.current_block_hash)
    node_a._orphans = {block.current_block_hash: block for block in (fake_2, fake_3, fake_4)}
    candidate = node_a._best_orphan_chain()
    reorg_accepted = node_a._try_reorg()
    check(
        "a longer branch below the finality checkpoint is rejected",
        candidate is not None
        and len(candidate) == 5
        and reorg_accepted is False
        and node_a.blockchain.tip.index == 4
        and node_a.finalized_height == 4,
        f"candidate_len={len(candidate) if candidate else None} reorg={reorg_accepted} tip={node_a.blockchain.tip.index}",
    )

    await node_b.stop()
    await node_a.stop()


async def wait_until(description, predicate, timeout=10.0, interval=0.2):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        outcome = predicate()
        if asyncio.iscoroutine(outcome):
            outcome = await outcome
        if outcome:
            return True
        await asyncio.sleep(interval)
    check(description, False, "timed out")
    return False


async def run_async(public_key, private_key):
    await lifecycle_checks(public_key, private_key)
    await proposer_checks(public_key, private_key)
    await slashing_checks(public_key, private_key)
    await finality_flow()


if __name__ == "__main__":
    PUBLIC_KEY, PRIVATE_KEY = setup_identity()
    asyncio.run(run_async(PUBLIC_KEY, PRIVATE_KEY))
    sys.exit(summary())
