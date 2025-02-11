from fastapi import APIRouter, Depends
from blockchain.Transaction import Transaction
from network.Node import Node

router = APIRouter()

node = Node()

@router.get("/sync")
def sync(last_seen_block_timestamp: float):
    blockchain_data = node.ask_synchronize(last_seen_block_timestamp)
    return {"blockchain": blockchain_data}

@router.post("/transaction")
def send_transaction(data: bytes):
    tx = Transaction(node.transaction_pool, data)
    node.send_transaction(tx)

    return {"status": "Transaction added", "fee": tx.fee}

@router.get("/bootstrap")
def get_bootstrap_peers():
    return {"peers": node.PEERS}

@router.post("/join")
def join():
    node.discover_peers()
    return {"status": "Node joined successfully"}
