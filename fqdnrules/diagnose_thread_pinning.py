# /// script
# requires-python = ">=3.9"
# dependencies = ["redis"]
# ///
"""Standalone diagnostic: does Dragonfly spread new client connections across
its proactor/IO threads, or do they pile onto one?

Background: Dragonfly is sharded internally across N threads ("proactors").
Each client connection is assigned to exactly one proactor at accept time
and stays there for its whole lifetime -- that thread does all the socket
I/O, RESP parsing, and command dispatch for that connection. If a benchmark
client's connections all land on the same proactor (regardless of how many
client-side threads or how much concurrency it uses), server CPU will look
capped at roughly 1/N of total capacity no matter how hard the client
pushes, because one thread's I/O throughput becomes the ceiling. This
script checks whether that's happening, and whether it depends on *how*
connections are established: all at once ("burst", e.g. via a
threading.Barrier) vs spread out over time ("stagger"), and whether they
share one connection-pool object (as a typical app client does) vs each
coming from an independent redis.Redis() instance.

Method: snapshot CLIENT LIST's connection ids before opening a batch of N
test connections, open them under a given (pool, timing) pattern, snapshot
CLIENT LIST again, diff to find the new connections, and read Dragonfly's
`tid=` field (the owning proactor) off each one. A real spread looks like
roughly N/threads connections per tid; pinning looks like all N landing on
one or two tids.

Requires the server to expose a per-connection thread id in CLIENT LIST
(Dragonfly's `tid=` extension). Stock Redis doesn't have this and doesn't
need this diagnostic -- it executes commands on one thread by design.

Usage (uv installs the one dependency -- `redis` -- into an ephemeral env
from the inline script metadata above, so nothing needs pre-installing):
    uv run diagnose_thread_pinning.py --host localhost --port 6379
    uv run diagnose_thread_pinning.py --uri rediss://default:pw@host:6385
    uv run diagnose_thread_pinning.py --host localhost --port 6379 \\
        --num-connections 32 --trials 3 --cases shared-burst,shared-stagger

Or without uv: `pip install redis && python3 diagnose_thread_pinning.py ...`

Single file, no other project files needed; safe to copy to any machine on
its own.
"""
from __future__ import annotations

import argparse
import threading
import time
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field

import redis

ALL_CASES = ("independent-burst", "independent-stagger", "shared-burst", "shared-stagger")


@dataclass
class CaseResult:
    case: str
    trial: int
    new_connection_count: int
    tid_counts: Counter = field(default_factory=Counter)

    @property
    def unique_threads(self) -> int:
        return len(self.tid_counts)


def make_client(host: str, port: int, db: int, uri: str | None) -> redis.Redis:
    if uri:
        return redis.Redis.from_url(uri)
    return redis.Redis(host=host, port=port, db=db)


def snapshot_by_id(observer: redis.Redis) -> dict[str, dict]:
    return {str(row["id"]): row for row in observer.client_list()}


def open_independent(
    host: str, port: int, db: int, uri: str | None, n: int, timing: str, stagger_ms: float
) -> list[redis.Redis]:
    clients = [make_client(host, port, db, uri) for _ in range(n)]
    if timing == "stagger":
        for c in clients:
            c.ping()
            time.sleep(stagger_ms / 1000.0)
    else:
        barrier = threading.Barrier(n)

        def connect_one(c: redis.Redis) -> None:
            barrier.wait()
            c.ping()

        with ThreadPoolExecutor(max_workers=n) as pool:
            list(pool.map(connect_one, clients))
    return clients


def close_independent(clients: list[redis.Redis]) -> None:
    for c in clients:
        c.close()


def open_shared(host: str, port: int, db: int, uri: str | None, n: int, timing: str, stagger_ms: float):
    """Check out n distinct raw connections from ONE shared redis.Redis()
    object's pool -- this is what a typical app client does under the hood
    every time a different thread issues a command (e.g. redis-py's
    ThreadPoolExecutor-driven callers): each thread borrows a connection
    from the pool, uses it, and the pool creates a new one if none are idle.
    """
    client = make_client(host, port, db, uri)

    def checkout_one():
        conn = client.connection_pool.get_connection()
        conn.send_command("PING")
        conn.read_response()
        return conn

    if timing == "stagger":
        conns = []
        for _ in range(n):
            conns.append(checkout_one())
            time.sleep(stagger_ms / 1000.0)
    else:
        barrier = threading.Barrier(n)

        def checkout_barriered(_: int):
            barrier.wait()
            return checkout_one()

        with ThreadPoolExecutor(max_workers=n) as pool:
            conns = list(pool.map(checkout_barriered, range(n)))
    return client, conns


def close_shared(client: redis.Redis, conns: list) -> None:
    for c in conns:
        try:
            client.connection_pool.release(c)
        except Exception:
            c.disconnect()
    client.close()


def run_case(
    observer: redis.Redis,
    host: str,
    port: int,
    db: int,
    uri: str | None,
    case: str,
    n: int,
    stagger_ms: float,
    settle_seconds: float,
) -> CaseResult:
    pool_mode, timing = case.split("-", 1)
    before = set(snapshot_by_id(observer))

    if pool_mode == "independent":
        held = open_independent(host, port, db, uri, n, timing, stagger_ms)
    else:
        held = open_shared(host, port, db, uri, n, timing, stagger_ms)

    time.sleep(settle_seconds)
    after = snapshot_by_id(observer)
    new_ids = [i for i in after if i not in before]
    tid_counts = Counter(after[i]["tid"] for i in new_ids if "tid" in after[i])

    if pool_mode == "independent":
        close_independent(held)
    else:
        close_shared(*held)

    return CaseResult(case=case, trial=0, new_connection_count=len(new_ids), tid_counts=tid_counts)


def verdict_for(result: CaseResult, expected_n: int) -> str:
    if result.new_connection_count == 0:
        return "NO NEW CONNECTIONS OBSERVED (check --settle-seconds or server reachability)"
    if "tid" not in "".join(result.tid_counts.keys()) and not result.tid_counts:
        return "server did not report a tid= field (not Dragonfly, or CLIENT LIST format differs)"
    if result.unique_threads <= 1:
        return f"PINNED -- all {result.new_connection_count} connections landed on ONE thread"
    if result.unique_threads < min(expected_n, 4):
        return f"PARTIALLY SPREAD -- only {result.unique_threads} distinct threads used"
    return f"SPREAD -- {result.unique_threads} distinct threads used"


def print_result(result: CaseResult, expected_n: int) -> None:
    hist = ", ".join(f"tid={t}:{c}" for t, c in sorted(result.tid_counts.items(), key=lambda kv: -kv[1]))
    print(f"  trial {result.trial}: {result.new_connection_count} new conns -> {hist or '(none)'}")
    print(f"    verdict: {verdict_for(result, expected_n)}")


def build_arg_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--host", default="localhost")
    parser.add_argument("--port", type=int, default=6379)
    parser.add_argument("--db", type=int, default=0)
    parser.add_argument("--uri", type=str, default=None, help="redis:// or rediss:// URI instead of host/port/db")
    parser.add_argument("--num-connections", type=int, default=20)
    parser.add_argument("--stagger-ms", type=float, default=30.0, help="delay between connects in *-stagger cases")
    parser.add_argument("--settle-seconds", type=float, default=0.3, help="pause after opening before snapshotting")
    parser.add_argument("--cooldown-seconds", type=float, default=1.0, help="pause between cases/trials")
    parser.add_argument("--trials", type=int, default=1, help="repeat each case this many times")
    parser.add_argument(
        "--cases",
        type=str,
        default=",".join(ALL_CASES),
        help=f"comma-separated subset of {ALL_CASES}",
    )
    return parser


def main() -> None:
    args = build_arg_parser().parse_args()
    cases = [c.strip() for c in args.cases.split(",") if c.strip()]
    for c in cases:
        if c not in ALL_CASES:
            raise SystemExit(f"unknown case {c!r}, expected one of {ALL_CASES}")

    observer = make_client(args.host, args.port, args.db, args.uri)
    observer.ping()

    server_info = observer.info("server")
    print(
        f"connected: redis_version={server_info.get('redis_version')} "
        f"dragonfly_version={server_info.get('dragonfly_version', 'n/a')} "
        f"thread_count={server_info.get('thread_count', 'n/a')}"
    )
    if "dragonfly_version" not in server_info:
        print("warning: server does not report dragonfly_version -- tid= pinning likely doesn't apply here")

    for case in cases:
        print(f"\n=== {case} (n={args.num_connections}) ===")
        for trial in range(1, args.trials + 1):
            result = run_case(
                observer, args.host, args.port, args.db, args.uri,
                case, args.num_connections, args.stagger_ms, args.settle_seconds,
            )
            result.trial = trial
            print_result(result, args.num_connections)
            time.sleep(args.cooldown_seconds)


if __name__ == "__main__":
    main()
