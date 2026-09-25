"""Background sales simulator.

Runs as an asyncio task alongside the request-serving event loop (the
demo's second "thread"). It attempts a sale for every snack on each tick,
faster than any configured rate, and calls back into the app over an
in-process ASGI transport so each attempt goes through real FastAPI
dependency injection -- including the rate-limited ``/api/sell`` route --
exactly as an external client's request would.
"""

from __future__ import annotations

import asyncio

import httpx
from fastapi import FastAPI

from .leaderboard import SNACK_RATES

# Faster than the quickest configured rate (Ice-Cream: 10/second) so the
# rate limiter -- not this loop -- is what caps the accepted sale rate.
TICK_SECONDS = 0.05


async def run_worker(app: FastAPI) -> None:
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://worker.internal") as client:
        while True:
            for snack in SNACK_RATES:
                await client.post(f"/api/sell/{snack}")
            await asyncio.sleep(TICK_SECONDS)
