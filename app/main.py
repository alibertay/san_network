import logging
from contextlib import asynccontextmanager

from fastapi import FastAPI

from app.limits import RateLimitMiddleware
from app.routes import router
from network.config import NodeConfig
from network.Node import Node

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s [%(name)s] %(message)s",
)

logger = logging.getLogger(__name__)


def create_app() -> FastAPI:
    """Build the API app; exposed so tests can use custom limits."""

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        """Start the P2P listeners with the application and stop them on shutdown."""
        node = Node()
        await node.start()
        app.state.node = node
        logger.info("SAN Network API ready (chain_id=%s)", node.chain_id)
        try:
            yield
        finally:
            await node.stop()
            logger.info("SAN Network API stopped")

    limits = NodeConfig.from_env()
    app = FastAPI(title="SAN Network API", lifespan=lifespan)
    app.add_middleware(
        RateLimitMiddleware,
        limit=limits.rpc_rate_limit,
        window=limits.rpc_rate_window,
        max_body=limits.rpc_max_body,
    )
    app.include_router(router)
    return app


app = create_app()
