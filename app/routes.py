from fastapi import APIRouter, Depends
from blockchain.Blockchain import Blockchain
from blockchain.Transaction import Transaction
from network.Node import Node

router = APIRouter()

node = Node()

@router.get("/sync")
def sync(last_seen_block_timestamp: float):
    """
    Ağdaki diğer node'larla senkronizasyon sağlar.
    """
    blockchain_data = node.ask_synchronize(last_seen_block_timestamp)
    return {"blockchain": blockchain_data}

@router.post("/transaction")
def send_transaction(data: bytes):
    """
    Yeni bir işlem ekler ve ücreti hesaplar.
    """
    tx = Transaction(node.transaction_pool, data)
    node.send_transaction(tx)

    return {"status": "Transaction added", "fee": tx.fee}

@router.get("/bootstrap")
def get_bootstrap_peers():
    """
    Yeni bir node ağa katıldığında bootstrap node'dan PEERS listesini alır.
    """
    return {"peers": node.PEERS}

@router.post("/join")
def join():
    """
    Node'un diğer ağ düğümlerini keşfetmesini sağlar.
    """
    node.discover_peers()
    return {"status": "Node joined successfully"}
