"""Public RPC hardening: per-IP rate limiting and request body limits."""

from __future__ import annotations

import time
from collections import defaultdict, deque

from starlette.middleware.base import BaseHTTPMiddleware
from starlette.responses import JSONResponse

MAX_TRACKED_CLIENTS = 10_000


class RateLimitMiddleware(BaseHTTPMiddleware):
    """Token-free sliding window limiter per client IP.

    Public nodes must not be able to be trivially flooded: each client IP gets
    ``limit`` requests per ``window`` seconds, and request bodies larger than
    ``max_body`` bytes are rejected with 413.
    """

    def __init__(self, app, *, limit: int, window: float, max_body: int):
        super().__init__(app)
        self.limit = max(int(limit), 1)
        self.window = max(float(window), 0.1)
        self.max_body = max(int(max_body), 1)
        self._hits: dict[str, deque[float]] = defaultdict(deque)

    def _client_key(self, request) -> str:
        if request.client is None:
            return "unknown"
        return request.client.host

    async def dispatch(self, request, call_next):
        content_length = request.headers.get("content-length")
        if content_length:
            try:
                if int(content_length) > self.max_body:
                    return JSONResponse(
                        {"detail": "request body too large"}, status_code=413
                    )
            except ValueError:
                return JSONResponse({"detail": "invalid content-length"}, status_code=400)

        client = self._client_key(request)
        now = time.monotonic()
        bucket = self._hits[client]
        while bucket and bucket[0] <= now - self.window:
            bucket.popleft()

        if len(bucket) >= self.limit:
            return JSONResponse(
                {"detail": "rate limit exceeded"}, status_code=429
            )

        bucket.append(now)
        if len(self._hits) > MAX_TRACKED_CLIENTS:
            self._hits = defaultdict(
                deque, {key: value for key, value in self._hits.items() if value}
            )

        return await call_next(request)
