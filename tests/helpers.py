"""Shared helpers for the SAN Network smoke test scripts."""
import asyncio
import json
import os
import socket
import time

from blockchain import crypto
from blockchain.address import address_from_public_key
from blockchain.merkle import state_root as compute_state_root
from blockchain.Transaction import Transaction
from network.config import NodeConfig
from utils import canonical

CHAIN_ID = "san-test"
SAN_BASE = 10 ** 8
FAILURES: list[str] = []


def check(name, condition, detail=""):
    status = "PASS" if condition else "FAIL"
    print(f"[{status}] {name}" + (f" :: {detail}" if detail else ""))
    if not condition:
        FAILURES.append(name)
    return bool(condition)


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def make_config(**overrides):
    base = dict(
        chain_id=CHAIN_ID,
        host="127.0.0.1",
        advertise_host="127.0.0.1",
        api_port=free_port(),
        p2p_port=free_port(),
        peer_port=free_port(),
        controller_port=free_port(),
        block_threshold_fee=0.0001,
        peer_check_interval=1.0,
        ws_timeout=3.0,
    )
    public_key = os.environ.get("PUBLIC_KEY")
    if public_key:
        base["genesis_allocations"] = {public_key: 1_000_000 * SAN_BASE}
    base.update(overrides)
    return NodeConfig(**base)


def setup_identity():
    """Create a node identity and expose it through the environment."""
    public_key, private_key = crypto.generate_keypair()
    os.environ["PUBLIC_KEY"] = public_key.hex()
    os.environ["PRIVATE_KEY"] = private_key.hex()
    # Also configure the app-level node (NodeConfig.from_env) with funds.
    os.environ["SAN_GENESIS_ALLOCATION"] = f"{public_key.hex()}:1000000"
    os.environ["SAN_CHAIN_ID"] = CHAIN_ID
    return public_key, private_key


def address_of(public_key) -> str:
    return address_from_public_key(public_key)


def seal_block(node, block):
    """Fill in the post-state root, hash and signature of a hand-built block.

    Mirrors what block production does, so tests exercising the honest path
    commit to the same state root a real proposer would.
    """
    outcome = node._simulate_block(block, apply=False)
    if outcome is None:
        return None
    block.state_root = compute_state_root(
        outcome["balances"],
        outcome["nonces"],
        outcome["validators"],
        outcome["total_slashed"],
        outcome["storage"],
        outcome["parameters"],
        outcome["base_fee"],
        outcome["total_burned"],
    )
    block.current_block_hash = block.calculate_hash()
    block.validator_signature = node._sign_block_hash(block.current_block_hash)
    return block


def sample_address(prefix: str = "ab") -> str:
    """A deterministic, valid address for receivers in tests."""
    body = (prefix * 40)[:40]
    return "0x" + body.lower()


def signed_payload(public_key, private_key, nonce, *, receiver=None, value=None, extra=None):
    payload = {
        "chain_id": CHAIN_ID,
        "sender": public_key.hex() if isinstance(public_key, bytes) else public_key,
        "nonce": nonce,
    }
    if receiver is not None:
        payload["receiver"] = receiver
    if value is not None:
        payload["value"] = value
    if extra:
        payload.update(extra)
    payload["signature"] = Transaction.sign_payload(payload, private_key)
    return payload


class FakePeerServer:
    """A fake gRPC P2P peer for transport tests.

    ``responder`` is an async callable ``responder(recv, send)`` where ``recv``
    returns the next decoded JSON message and ``send`` queues a JSON reply.
    The handshake is answered automatically.
    """

    def __init__(self, identity, responder, port: int, status: dict | None = None):
        self.identity = identity
        self.responder = responder
        self.port = port
        self.status = status
        self._server = None

    async def start(self) -> None:
        import grpc

        from network.proto import p2p_pb2, p2p_pb2_grpc

        outer = self

        class Servicer(p2p_pb2_grpc.P2PServicer):
            async def Status(self, request, context):  # noqa: N802
                if outer.status is None:
                    await context.abort(grpc.StatusCode.UNIMPLEMENTED, "no status")
                return p2p_pb2.Envelope(payload=json.dumps(outer.status).encode("utf-8"))

            async def Session(self, request_iterator, context):  # noqa: N802
                inbound: asyncio.Queue = asyncio.Queue()
                outbound: asyncio.Queue = asyncio.Queue()

                async def reader():
                    async for envelope in request_iterator:
                        await inbound.put(json.loads(envelope.payload.decode("utf-8")))
                    await inbound.put(None)

                async def recv():
                    item = await inbound.get()
                    if item is None:
                        raise ConnectionError("client closed")
                    return item

                async def send(payload):
                    await outbound.put(payload)

                async def driver():
                    try:
                        hello = await recv()
                        if hello.get("type") != "HELLO":
                            raise AssertionError(f"expected HELLO, got {hello!r}")
                        record = {
                            "type": "HELLO_ACK",
                            "protocol": hello.get("protocol"),
                            "chain_id": hello.get("chain_id", CHAIN_ID),
                            "public_key": outer.identity.public_key_hex,
                            "timestamp": time.time(),
                        }
                        record["signature"] = outer.identity.sign_hex(
                            canonical.dumps_bytes(record)
                        )
                        await send(record)
                        await outer.responder(recv, send)
                    except Exception:  # noqa: BLE001 - fake peer just stops
                        pass
                    finally:
                        await outbound.put(None)

                reader_task = asyncio.create_task(reader())
                driver_task = asyncio.create_task(driver())
                try:
                    while True:
                        item = await outbound.get()
                        if item is None:
                            break
                        yield p2p_pb2.Envelope(
                            payload=json.dumps(item).encode("utf-8")
                        )
                finally:
                    driver_task.cancel()
                    reader_task.cancel()

        self._server = grpc.aio.server()
        p2p_pb2_grpc.add_P2PServicer_to_server(Servicer(), self._server)
        self._server.add_insecure_port(f"127.0.0.1:{self.port}")
        await self._server.start()

    async def stop(self) -> None:
        if self._server is not None:
            await self._server.stop(0)


async def accept_handshake(websocket, identity, chain_id: str = CHAIN_ID):
    """Deprecated websocket helper retained for older tests."""
    raw = await websocket.recv()
    data = json.loads(raw)
    record = {
        "type": "HELLO_ACK",
        "protocol": data.get("protocol"),
        "chain_id": data.get("chain_id", chain_id),
        "public_key": identity.public_key_hex,
        "timestamp": time.time(),
    }
    record["signature"] = identity.sign_hex(canonical.dumps_bytes(record))
    await websocket.send(json.dumps(record))


def summary() -> int:
    print()
    if FAILURES:
        print(f"{len(FAILURES)} FAILURE(S): {FAILURES}")
        return 1
    print("ALL CHECKS PASSED")
    return 0
