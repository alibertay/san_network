import asyncio
import logging

from fastapi import APIRouter, Body, Depends, HTTPException, Query, Request
from fastapi.responses import PlainTextResponse
from pydantic import BaseModel, Field

from blockchain.Block import SCHEMA_VERSION
from network.Node import Node

logger = logging.getLogger(__name__)

router = APIRouter()


def get_node(request: Request) -> Node:
    node: Node | None = getattr(request.app.state, "node", None)
    if node is None:
        raise HTTPException(status_code=503, detail="Node is not started")
    return node


class ContractQuery(BaseModel):
    contract_id: str
    function_name: str
    params: list = Field(default_factory=list)


# ---------------------------------------------------------------------- #
# Status and queries
# ---------------------------------------------------------------------- #

@router.get("/health")
async def health(node: Node = Depends(get_node)):
    tip = node.blockchain.tip
    return {
        "status": "ok",
        "chain_id": node.chain_id,
        "schema_version": SCHEMA_VERSION,
        "height": tip.index,
        "tip_hash": tip.current_block_hash,
        "state_root": tip.state_root,
        "base_fee": node.blockchain.base_fee,
        "finalized_height": node.finalized_height,
        "finalized_hash": node.finalized_hash,
        "peers": len(node.PEERS),
        "controllers": len(node.controller_nodes),
        "mempool": len(node.transaction_pool),
        "contracts": len(node.storage.contracts),
        "validators": len(node.blockchain.active_validators()),
    }


@router.get("/validators")
async def get_validators(node: Node = Depends(get_node)):
    """Active validator set, stake weights and finality checkpoint."""
    return node.attestation_state()


@router.get("/genesis")
async def get_genesis(node: Node = Depends(get_node)):
    """Public genesis information; a joiner needs it to reproduce the chain.

    Genesis is defined by the chain id, the initial allocation and the
    consensus parameters, all of which are public on-chain state.
    """
    chain = node.blockchain
    return {
        "chain_id": node.chain_id,
        "schema_version": SCHEMA_VERSION,
        "genesis_hash": chain.chain[0].current_block_hash if chain.chain else None,
        "genesis_allocation": {
            address: str(units)
            for address, units in chain.genesis_allocations.items()
        },
        # Immutable genesis parameters: current (mutable) parameters would
        # make a joiner rebuild a different genesis after any governance change.
        "parameters": {
            key: str(value) for key, value in chain.genesis_parameters.items()
        },
    }


@router.get("/finality")
async def get_finality(node: Node = Depends(get_node)):
    tip = node.blockchain.tip
    return {
        "chain_id": node.chain_id,
        "height": tip.index,
        "tip_hash": tip.current_block_hash,
        "finalized_height": node.finalized_height,
        "finalized_hash": node.finalized_hash,
        "pending_vote_heights": node.pending_finality_heights(),
    }


@router.get("/evidence")
async def get_evidence(node: Node = Depends(get_node)):
    """Collected equivocation evidence that can be submitted as a transaction."""
    return {"evidence": node.equivocation_evidence()}


# ---------------------------------------------------------------------- #
# Light client support
# ---------------------------------------------------------------------- #

@router.get("/headers")
async def get_headers(
    from_index: int = Query(0, ge=0),
    limit: int = Query(64, ge=1, le=512),
    node: Node = Depends(get_node),
):
    """Header chain (no transactions) for light clients."""
    return {"headers": node.get_headers(from_index, limit)}


@router.get("/proof/account/{address}")
async def get_account_proof(address: str, node: Node = Depends(get_node)):
    """Merkle inclusion proof of an account against the current state root."""
    try:
        proof = await asyncio.to_thread(node.get_account_proof, address)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    if proof is None:
        raise HTTPException(status_code=404, detail=f"No account proof for {address}")
    return proof


@router.get("/proof/tx/{block_index}/{tx_index}")
async def get_transaction_proof(
    block_index: int, tx_index: int, node: Node = Depends(get_node)
):
    """Merkle inclusion proof of a transaction against a block's tx_root."""
    proof = node.get_transaction_proof(block_index, tx_index)
    if proof is None:
        raise HTTPException(status_code=404, detail="No such transaction")
    return proof


@router.get("/snapshot")
async def get_snapshot(node: Node = Depends(get_node)):
    """Latest finalized state snapshot (verifiable against the block header)."""
    snapshot = node.finalized_snapshot()
    if snapshot is None:
        raise HTTPException(status_code=404, detail="No finalized snapshot yet")
    return snapshot


@router.get("/receipt/{block_index}/{tx_index}")
async def get_receipt(block_index: int, tx_index: int, node: Node = Depends(get_node)):
    """Execution receipt (status, gas used, logs) for a recent transaction."""
    receipt = node.get_receipt(block_index, tx_index)
    if receipt is None:
        raise HTTPException(status_code=404, detail="No receipt available")
    return receipt


@router.get("/tx/{tx_id}")
async def get_transaction(tx_id: str, node: Node = Depends(get_node)):
    """Transaction lookup (with its block and receipt) through the tx index."""
    transaction = await asyncio.to_thread(node.get_transaction, tx_id)
    if transaction is None:
        raise HTTPException(status_code=404, detail=f"Unknown transaction {tx_id}")
    return transaction


@router.get("/receipt/tx/{tx_id}")
async def get_receipt_by_tx(tx_id: str, node: Node = Depends(get_node)):
    """Receipt lookup by transaction id."""
    transaction = await asyncio.to_thread(node.get_transaction, tx_id)
    if transaction is None or transaction.get("receipt") is None:
        raise HTTPException(status_code=404, detail=f"No receipt for {tx_id}")
    return transaction["receipt"]


@router.get("/metrics")
async def get_metrics(node: Node = Depends(get_node)):
    """Prometheus text exposition of node counters and gauges."""
    from app.metrics import render_metrics

    return PlainTextResponse(
        render_metrics(node.metrics_snapshot()),
        media_type="text/plain; version=0.0.4",
    )


@router.get("/account/{address}")
async def get_account(address: str, node: Node = Depends(get_node)):
    try:
        return node.get_account(address)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc


@router.get("/block/{index}")
async def get_block(index: int, node: Node = Depends(get_node)):
    block = node.get_block(index)
    if block is None:
        raise HTTPException(status_code=404, detail=f"Block {index} not found")
    return block


@router.get("/mempool")
async def get_mempool(node: Node = Depends(get_node)):
    return {
        "count": len(node.transaction_pool),
        "tx_ids": [tx.tx_id for tx in node.transaction_pool],
    }


@router.get("/contracts")
async def get_contracts(node: Node = Depends(get_node)):
    return {"contracts": node.list_contracts()}


@router.post("/contract/query")
async def query_contract(query: ContractQuery, node: Node = Depends(get_node)):
    try:
        result = await asyncio.to_thread(
            node.query_contract, query.contract_id, query.function_name, query.params
        )
    except ValueError as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc
    except Exception as exc:  # noqa: BLE001 - VM errors are user errors here
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return {"result": result}


# ---------------------------------------------------------------------- #
# Chain and peer management
# ---------------------------------------------------------------------- #

@router.get("/sync")
async def sync(
    from_index: int = Query(0, ge=0),
    limit: int = Query(128, ge=1, le=1024),
    node: Node = Depends(get_node),
):
    """One page of blocks (index >= from_index) plus a state snapshot.

    Follow ``has_more`` / ``next_from_index`` until caught up. The snapshot is
    informational: peers rebuild state by replaying the verified blocks, never
    by trusting this payload.
    """
    return node.get_sync_payload(from_index, limit)


@router.post("/transaction")
async def send_transaction(payload: dict = Body(...), node: Node = Depends(get_node)):
    """Accept a transaction, pool it and produce a block when fees allow."""
    try:
        return await node.submit_transaction(payload)
    except (TypeError, ValueError) as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc


@router.get("/bootstrap")
async def get_bootstrap_peers(node: Node = Depends(get_node)):
    """Known peers plus this node's own signed record (so a fresh node can join)."""
    return {"peers": [*node.PEERS, node.self_peer_record()]}


@router.post("/join")
async def join(node: Node = Depends(get_node)):
    """Discover peers from the configured bootstrap node and register with them."""
    bootstrap = node.config.bootstrap
    if not bootstrap:
        raise HTTPException(
            status_code=400,
            detail="No bootstrap node configured (set SAN_BOOTSTRAP)",
        )

    peers = await asyncio.to_thread(node.discover_peers, bootstrap)
    added = node.add_peers(peers)
    if node.PEERS:
        await node.register_to_network()

    return {"status": "joined", "new_peers": added, "peers": node.PEERS}
