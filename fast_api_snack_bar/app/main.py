"""Snack sales demo: fastapi-redis-sdk backed by Dragonfly.

- POST /api/sell/{snack}       Service-layer sale, gated by a DI rate limiter.
- GET  /api/leaderboard        Real-time leaderboard read (panel 1).
- GET  /api/leaderboard/cached DI-cached read, up to 5s stale (panel 2).
"""

from __future__ import annotations

import argparse
import asyncio
import contextlib
import os
from contextlib import asynccontextmanager
from pathlib import Path
from typing import AsyncIterator
from urllib.parse import urlparse

from fastapi import Depends, FastAPI, HTTPException
from fastapi.responses import HTMLResponse
from redis_fastapi import AsyncRedisDep, FastAPIRedis, RateLimitBackendDep, cache

from . import leaderboard
from .worker import run_worker

STATIC_DIR = Path(__file__).parent / "static"


def _apply_uri_arg() -> None:
    """Point the SDK's env-var-driven settings at -uri, rediss:// implying TLS.

    Must run before FastAPIRedis/cache() touch redis_fastapi's settings below:
    get_settings() is @lru_cache'd on first read, so setting REDIS_URL any
    later would be a no-op.
    """
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "-uri", dest="uri", help="Redis connection URI, e.g. redis://host:6379 or rediss://host:6380"
    )
    args, _ = parser.parse_known_args()
    if not args.uri:
        return
    os.environ["REDIS_URL"] = args.uri
    if urlparse(args.uri).scheme == "rediss":
        os.environ["REDIS_SSL"] = "true"


if __name__ == "__main__":
    _apply_uri_arg()


@asynccontextmanager
async def lifespan(app: FastAPI) -> AsyncIterator[None]:
    # Runs inside FastAPIRedis's own lifespan (added below), so the Redis
    # pool is already up by the time the worker makes its first request.
    task = asyncio.create_task(run_worker(app))
    try:
        yield
    finally:
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task


app = FastAPI(title="Snack Sales Demo", lifespan=lifespan)
FastAPIRedis(app).lifespan().caching().rate_limiting()


@app.get("/", response_class=HTMLResponse)
async def index() -> str:
    return (STATIC_DIR / "index.html").read_text()


@app.post("/api/sell/{snack}")
async def sell(snack: str, limiter: RateLimitBackendDep, redis: AsyncRedisDep) -> dict:
    """Record one attempted sale, capped at the snack's configured rate.

    The rate limiter is the enforcement point: it is consulted on every
    attempt, and the leaderboard is only incremented when it allows the
    request through, which is what keeps each snack's growth at its target
    rate even though the worker calls this far more often than that.
    """
    rate = leaderboard.SNACK_RATES.get(snack)
    if rate is None:
        raise HTTPException(status_code=404, detail=f"unknown snack {snack!r}")

    result = await limiter.hit(
        "worker", limit=rate.limit, window=rate.window, scope=f"sell:{snack}"
    )
    if result.allowed:
        await leaderboard.record_sale(redis, snack)
    return {"snack": snack, "sold": result.allowed, "remaining": result.remaining}


@app.get("/api/leaderboard")
async def get_leaderboard(redis: AsyncRedisDep) -> list[dict]:
    return await leaderboard.read_leaderboard(redis)


@app.get(
    "/api/leaderboard/cached",
    dependencies=[Depends(cache(ttl=5))],
)
async def get_leaderboard_cached(redis: AsyncRedisDep) -> list[dict]:
    return await leaderboard.read_leaderboard(redis)


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=8000)
