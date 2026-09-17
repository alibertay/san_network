"""Single-node entry point.

IMPORTANT: SAN keeps the chain in this process (plus the optional LMDB
database), so the API must run with exactly ONE worker. Multiple uvicorn
workers would each build their own chain and diverge. Scale out with separate
nodes and P2P, not with uvicorn workers.

Environment highlights (see network/config.py for the full list):
    SAN_API_PORT=8000           REST port
    SAN_DB_PATH=data/node.kv    enable LMDB persistence
    SAN_KEY_FILE=san_key.json   persistent node identity
    SAN_BOOTSTRAP=host:port     seed node to join
    SAN_GENESIS_ALLOCATION=addr:1000000[,addr2:500]
"""

from app.main import app
from network.config import NodeConfig


def main() -> None:
    import uvicorn

    config = NodeConfig.from_env()
    uvicorn.run(app, host=config.host, port=config.api_port, workers=1)


if __name__ == "__main__":
    main()
