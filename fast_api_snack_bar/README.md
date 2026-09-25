# Snack sales demo

FastAPI + [fastapi-redis-sdk](https://github.com/redis/fastapi-redis-sdk) backed by Dragonfly.

## Run

```
docker run -d --name dragonfly -p 6379:6379 docker.dragonflydb.io/dragonflydb/dragonfly
uv venv
source .venv/bin/activate
uv pip install -r requirements.txt
uv run uvicorn app.main:app --reload
```

Open http://localhost:8000. The app's default settings point at
`localhost:6379`, matching the container above; set `REDIS_URL` to point
elsewhere, or run `uv run python -m app.main -uri redis://host:6379` instead of
`uvicorn`. A `rediss://` URI turns on TLS.

## What it does

A background task simulates snack sales and calls `POST /api/sell/{snack}`
far faster than any snack's target rate. That route injects
`fastapi-redis-sdk`'s `RateLimitBackend` and only records the sale
(a Redis sorted-set increment) when the limiter allows it -- 1/s for
Apples, 2/s for Bananas, 10/s for Ice-Cream -- so the limiter is what
actually enforces the rates, not the loop's timing.

The page polls two endpoints side by side: `/api/leaderboard` reads the
sorted set directly, and `/api/leaderboard/cached` is wrapped in the SDK's
`cache()` dependency with a 5-second TTL, so it lags the live panel by up
to 5 seconds.
