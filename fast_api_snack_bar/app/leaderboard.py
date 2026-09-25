"""Service layer for the snacks_sold leaderboard.

Backed by a single Redis sorted set: one member per snack, score is the
running sale count. ``SNACK_RATES`` is the registry of the sale rate each
snack is allowed to grow at, keyed the same way as the sorted set members
so the rate limiter's scope and the leaderboard's identity never drift apart.
"""

from __future__ import annotations

from redis.asyncio import Redis

from redis_fastapi import Rate

LEADERBOARD_KEY = "snacks_sold"

SNACK_RATES: dict[str, Rate] = {
    "Apples": Rate.per_second(1),
    "Bananas": Rate.per_second(2),
    "Ice-Cream": Rate.per_second(10),
}


async def record_sale(redis: Redis, snack: str) -> None:
    await redis.zincrby(LEADERBOARD_KEY, 1, snack)


async def read_leaderboard(redis: Redis) -> list[dict[str, str | int]]:
    """Return all three snacks, ranked by count, highest first.

    Uses ZMSCORE against the known snack names rather than ZREVRANGE so a
    snack with zero sales still appears in the listing.
    """
    snacks = list(SNACK_RATES)
    scores = await redis.zmscore(LEADERBOARD_KEY, snacks)
    rows = [
        {"snack": snack, "count": int(score or 0)}
        for snack, score in zip(snacks, scores)
    ]
    rows.sort(key=lambda row: row["count"], reverse=True)
    return rows
