"""gRPC transport for node-to-node communication.

The consensus wire format is unchanged: every message is the same JSON
envelope that used to travel over WebSocket. gRPC (a bidirectional stream plus
a few unary RPCs) replaces the REST/socket transport; the public REST API is
only for users and clients.

Design: :class:`PeerStream` exposes a small ``recv``/``send``/``close`` surface
so the consensus code is transport-agnostic, and the server binds the same
service to every P2P port (peer, p2p and controller) for compatibility with
existing peer records.
"""

from __future__ import annotations

import asyncio
import json
import logging
from typing import Any, AsyncIterator

import grpc

from network.proto import p2p_pb2, p2p_pb2_grpc

logger = logging.getLogger(__name__)

# Per-session buffers are bounded so a flooding peer applies backpressure
# instead of growing memory; the semaphore caps concurrent sessions.
SESSION_QUEUE_SIZE = 256
MAX_CONCURRENT_SESSIONS = 256

# gRPC channel option name for the target-name override used with self-signed
# certificates (SAN_TLS_CA deployments).
_SSL_TARGET_OVERRIDE = "grpc.ssl_target_name_override"


class PeerClosed(ConnectionError):
    """Raised by ``PeerStream.recv`` when the other side closed the session."""


class PeerStream:
    """A JSON-message session over a gRPC bidirectional stream."""

    def __init__(
        self,
        *,
        inbound: asyncio.Queue,
        outbound: asyncio.Queue,
        close_callback,
        label: str = "<peer>",
        reader_task: asyncio.Task | None = None,
    ):
        self._inbound = inbound
        self._outbound = outbound
        self._close_callback = close_callback
        self._reader_task = reader_task
        self.label = label
        self.closed = False

    # ------------------------------------------------------------------ #
    # Data path
    # ------------------------------------------------------------------ #

    async def recv(self) -> str:
        if self.closed:
            raise PeerClosed(f"{self.label}: session closed")
        item = await self._inbound.get()
        if item is _CLOSED:
            raise PeerClosed(f"{self.label}: session closed")
        return item

    async def send(self, data: str) -> None:
        if self.closed:
            raise PeerClosed(f"{self.label}: session closed")
        await self._outbound.put(data)

    async def close(self, code: int | None = None, reason: str | None = None) -> None:
        if self.closed:
            return
        self.closed = True
        # Half-close the outbound generator so the peer can finish processing
        # the last message before the RPC is torn down. Bounded wait: if the
        # queue is stuck full the stream is dead anyway.
        try:
            await asyncio.wait_for(self._outbound.put(_CLOSED), timeout=1.0)
        except (asyncio.QueueFull, asyncio.TimeoutError):
            pass
        try:
            await self._close_callback()
        except Exception as exc:  # noqa: BLE001 - closing must never raise
            logger.debug("Peer close failed (%s): %s", self.label, exc)

    # ------------------------------------------------------------------ #
    # Async iteration (``async for raw in stream``)
    # ------------------------------------------------------------------ #

    def __aiter__(self) -> "PeerStream":
        return self

    async def __anext__(self) -> str:
        try:
            return await self.recv()
        except PeerClosed:
            raise StopAsyncIteration from None

    def __bool__(self) -> bool:  # mirrors websocket objects in the old code
        return True


_CLOSED = object()


async def _envelope_stream(queue: asyncio.Queue) -> AsyncIterator[p2p_pb2.Envelope]:
    """Yield outbound envelopes until a sentinel closes the stream."""
    while True:
        item = await queue.get()
        if item is _CLOSED:
            return
        yield p2p_pb2.Envelope(payload=str(item).encode("utf-8"))


# ---------------------------------------------------------------------- #
# Server side
# ---------------------------------------------------------------------- #


class P2PServicer(p2p_pb2_grpc.P2PServicer):
    def __init__(self, node):
        self.node = node
        self._sessions = asyncio.Semaphore(MAX_CONCURRENT_SESSIONS)
        self._sync_calls = asyncio.Semaphore(8)

    async def Session(self, request_iterator, context):  # noqa: N802 (gRPC name)
        # Acquire the session slot BEFORE allocating buffers or reader tasks,
        # so flooding connections cannot accumulate work while queued.
        async with self._sessions:
            label = f"{context.peer()}"
            inbound: asyncio.Queue = asyncio.Queue(maxsize=SESSION_QUEUE_SIZE)
            outbound: asyncio.Queue = asyncio.Queue(maxsize=SESSION_QUEUE_SIZE)

            async def reader() -> None:
                try:
                    async for envelope in request_iterator:
                        await inbound.put(envelope.payload.decode("utf-8"))
                except Exception as exc:  # noqa: BLE001 - peer vanished
                    logger.debug("Session reader for %s ended: %s", label, exc)
                finally:
                    # The queue may be full with nobody consuming it during
                    # cancellation; never block the finalizer.
                    try:
                        inbound.put_nowait(_CLOSED)
                    except asyncio.QueueFull:
                        pass

            reader_task = asyncio.create_task(reader())

            async def close_callback() -> None:
                reader_task.cancel()
                try:
                    inbound.put_nowait(_CLOSED)
                except asyncio.QueueFull:
                    pass

            stream = PeerStream(
                inbound=inbound,
                outbound=outbound,
                close_callback=close_callback,
                label=label,
                reader_task=reader_task,
            )
            # The session loop and the outbound generator must run
            # concurrently: handshake replies are queued while the loop is
            # still reading.
            session_task = asyncio.create_task(self.node._peer_session(stream))
            try:
                async for envelope in _envelope_stream(outbound):
                    yield envelope
            finally:
                if not session_task.done():
                    session_task.cancel()
                if not reader_task.done():
                    reader_task.cancel()
                # Await the cleanup while the session slot is still held, so no
                # task outlives its connection.
                await asyncio.gather(session_task, reader_task, return_exceptions=True)

    async def Sync(self, request, context):  # noqa: N802
        # Bound the work a single (unauthenticated) call can trigger: page size
        # is capped and concurrent sync sessions are limited.
        limit = min(max(int(request.limit or 128), 1), 512)
        async with self._sync_calls:
            payload = self.node.get_sync_payload(int(request.from_index), limit)
        return p2p_pb2.SyncResponse(payload=json.dumps(payload).encode("utf-8"))

    async def Bootstrap(self, request, context):  # noqa: N802
        payload = {"peers": self.node.PEERS}
        return p2p_pb2.Envelope(payload=json.dumps(payload).encode("utf-8"))

    async def Status(self, request, context):  # noqa: N802
        payload = self.node.peer_status()
        return p2p_pb2.Envelope(payload=json.dumps(payload).encode("utf-8"))


def build_server(node) -> grpc.aio.Server:
    """Create the gRPC server (one service bound to every P2P port)."""
    options = [
        ("grpc.max_receive_message_length", node.config.ws_max_size),
        ("grpc.max_send_message_length", node.config.ws_max_size),
    ]
    server = grpc.aio.server(options=options)
    p2p_pb2_grpc.add_P2PServicer_to_server(P2PServicer(node), server)
    credentials = node.transport_server_credentials()
    for port in (node.config.peer_port, node.config.p2p_port, node.config.controller_port):
        if credentials is not None:
            server.add_secure_port(f"{node.config.host}:{port}", credentials)
        else:
            server.add_insecure_port(f"{node.config.host}:{port}")
    return server


# ---------------------------------------------------------------------- #
# Client side
# ---------------------------------------------------------------------- #


def _channel_options(peer: dict, max_size: int) -> list[tuple[str, Any]]:
    options: list[tuple[str, Any]] = [
        ("grpc.max_receive_message_length", max_size),
        ("grpc.max_send_message_length", max_size),
    ]
    if peer.get("tls"):
        # Self-signed certificates are identified by their host, not by a SAN
        # extension, so pin the expected target name explicitly.
        options.append((_SSL_TARGET_OVERRIDE, str(peer.get("host"))))
    return options


class PeerConnectionError(ConnectionError):
    """Raised when a peer session cannot be opened or fails its handshake."""


async def open_session(
    node, peer: dict, port_key: str, timeout: float
) -> PeerStream:
    """Open a gRPC session to ``peer[port_key]`` and complete the handshake."""
    address = f"{peer['host']}:{peer[port_key]}"
    options = _channel_options(peer, node.config.ws_max_size)
    if peer.get("tls"):
        channel = grpc.aio.secure_channel(
            address, node.transport_client_credentials(peer), options=options
        )
    else:
        channel = grpc.aio.insecure_channel(address, options=options)

    try:
        await asyncio.wait_for(channel.channel_ready(), timeout=timeout)
    except Exception as exc:
        await channel.close()
        raise PeerConnectionError(f"{address}: not reachable ({exc})") from exc

    outbound: asyncio.Queue = asyncio.Queue(maxsize=SESSION_QUEUE_SIZE)
    inbound: asyncio.Queue = asyncio.Queue(maxsize=SESSION_QUEUE_SIZE)

    stub = p2p_pb2_grpc.P2PStub(channel)
    stream_call = stub.Session(_envelope_stream(outbound))

    async def reader() -> None:
        try:
            async for envelope in stream_call:
                await inbound.put(envelope.payload.decode("utf-8"))
        except Exception as exc:  # noqa: BLE001 - remote closed
            logger.debug("Client session reader ended: %s", exc)
        finally:
            await inbound.put(_CLOSED)

    reader_task = asyncio.create_task(reader())

    async def close_callback() -> None:
        # Wait for the server to end the stream on its own after seeing the
        # half-close; only then tear the channel down.
        if not reader_task.done():
            try:
                await asyncio.wait_for(asyncio.shield(reader_task), timeout=2.0)
            except Exception:  # noqa: BLE001 - timeout or remote reset
                reader_task.cancel()
        if not stream_call.done():
            stream_call.cancel()
        await channel.close()

    stream = PeerStream(
        inbound=inbound,
        outbound=outbound,
        close_callback=close_callback,
        label=address,
        reader_task=reader_task,
    )

    try:
        await stream.send(json.dumps(node._hello_payload("HELLO")))
        raw = await asyncio.wait_for(stream.recv(), timeout=timeout * 2)
        if not node._verify_hello(json.loads(raw)):
            raise PeerConnectionError("handshake rejected by peer")
    except Exception:
        await stream.close()
        raise
    return stream


async def remote_sync(
    node, peer: dict, from_index: int, limit: int, timeout: float = 10.0
) -> dict:
    """Fetch one page of blocks from a peer over the Sync RPC."""
    address = f"{peer['host']}:{peer['p2p_port']}"
    if peer.get("tls"):
        credentials = node.transport_client_credentials(peer)
        channel = grpc.aio.secure_channel(
            address, credentials, options=_channel_options(peer, node.config.ws_max_size)
        )
    else:
        channel = grpc.aio.insecure_channel(
            address, options=_channel_options(peer, node.config.ws_max_size)
        )
    try:
        stub = p2p_pb2_grpc.P2PStub(channel)
        response = await asyncio.wait_for(
            stub.Sync(p2p_pb2.SyncRequest(from_index=from_index, limit=limit)),
            timeout=timeout,
        )
        return json.loads(response.payload.decode("utf-8"))
    finally:
        await channel.close()


async def remote_status(node, peer: dict, timeout: float = 5.0) -> dict | None:
    """Ask a peer for its chain id, height and finality (longest-chain pick)."""
    address = f"{peer['host']}:{peer['p2p_port']}"
    if peer.get("tls"):
        channel = grpc.aio.secure_channel(
            address,
            node.transport_client_credentials(peer),
            options=_channel_options(peer, node.config.ws_max_size),
        )
    else:
        channel = grpc.aio.insecure_channel(
            address, options=_channel_options(peer, node.config.ws_max_size)
        )
    try:
        stub = p2p_pb2_grpc.P2PStub(channel)
        response = await asyncio.wait_for(
            stub.Status(p2p_pb2.Empty()), timeout=timeout
        )
        return json.loads(response.payload.decode("utf-8"))
    except Exception as exc:  # noqa: BLE001 - unreachable peer is a valid outcome
        logger.debug("Status request to %s failed: %s", address, exc)
        return None
    finally:
        await channel.close()


async def remote_bootstrap(node, address: str, timeout: float = 5.0) -> list:
    """Ask a seed for its peer list over the Bootstrap RPC (gRPC-only path)."""
    if ":" not in address:
        raise PeerConnectionError(f"invalid bootstrap address {address!r}")
    host, _, port = address.rpartition(":")
    channel = grpc.aio.insecure_channel(
        f"{host}:{port}",
        options=[("grpc.max_receive_message_length", node.config.ws_max_size)],
    )
    try:
        stub = p2p_pb2_grpc.P2PStub(channel)
        response = await asyncio.wait_for(stub.Bootstrap(p2p_pb2.Empty()), timeout=timeout)
        return json.loads(response.payload.decode("utf-8")).get("peers") or []
    finally:
        await channel.close()
