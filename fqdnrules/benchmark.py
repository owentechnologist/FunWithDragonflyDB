"""CLI benchmark for RuleStore (fqdn_router.py): write N rules, run M timed
lookups against them, and report write throughput plus lookup latency
percentiles.
"""
from __future__ import annotations

import argparse
import bisect
import itertools
import math
import random
import sys
import threading
import time
import urllib.parse
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path

import redis

from fqdn_pattern import MatchMode
from fqdn_router import Rule, RuleStore

_PORTS = (443, 8443, 80)

NUM_DOMAINS = 10_000
"""Every key's fqdn1 apex is one of NUM_DOMAINS distinct domains, so a run
spreads keys across NUM_DOMAINS Dragonfly hash tags ({bench:dragonflyN.com})
instead of piling every key onto the single {bench:dragonfly.com} tag -- the
latter would pin all writes/lookups to one shard and hide the parallelism
Dragonfly is meant to demonstrate.
"""

DEFAULT_DEPTH_TIERS = "50:1-3,30:4-20,10:21-50,10:51-2000"

LAYOUT_SEED = 1_337
"""Fixed seed for partitioning rules into keys (see build_key_sizes):
independent of --seed so the key layout depends only on --num-rules and
--rule-depth-tiers, never on --seed -- preserving --no-flush's contract
that a rerun with a different --seed resamples queries against the same
previously-written keys instead of silently querying a different layout.
"""

Query = tuple[str, int, str]


@dataclass(frozen=True, slots=True)
class DepthTier:
    """One weighted band of the rule-depth distribution: this fraction of
    keys get a rule count sampled uniformly from [min_size, max_size]."""
    weight: float
    min_size: int
    max_size: int


def parse_depth_tiers(spec: str) -> list[DepthTier]:
    """Parse "PCT:LO-HI,PCT:LO-HI,..." into weighted key-depth tiers, e.g.
    "50:1-3,30:4-20,10:21-50,10:51-2000" models the real skew where a
    standard-zone key usually maps to a handful of endpoints/routes/rule
    exemptions, but a few map to many hundreds. Percentages need not sum to
    100 -- they're renormalized into weights -- but must be positive.
    """
    tiers = []
    for part in spec.split(","):
        pct_str, sep, range_str = part.partition(":")
        lo_str, sep2, hi_str = range_str.partition("-")
        if not sep or not sep2:
            raise ValueError(f"invalid depth tier {part!r}, expected PCT:LO-HI")
        try:
            pct = float(pct_str)
            lo = int(lo_str)
            hi = int(hi_str)
        except ValueError:
            raise ValueError(f"invalid depth tier {part!r}, expected PCT:LO-HI")
        if pct <= 0:
            raise ValueError(f"depth tier percentage must be > 0, got {part!r}")
        if lo < 1 or hi < lo:
            raise ValueError(f"depth tier range must satisfy 1 <= lo <= hi, got {part!r}")
        tiers.append(DepthTier(pct, lo, hi))
    if not tiers:
        raise ValueError("no depth tiers specified")
    total_pct = sum(t.weight for t in tiers)
    return [DepthTier(t.weight / total_pct, t.min_size, t.max_size) for t in tiers]


def build_key_sizes(num_rules: int, tiers: list[DepthTier], rng: random.Random) -> list[int]:
    """Partition num_rules rules into keys ("standard zones"), one size per
    key, each size sampled from a weighted pick among tiers then a uniform
    draw within that tier's [min_size, max_size]. The last key is trimmed so
    sizes sum to exactly num_rules.
    """
    weights = [t.weight for t in tiers]
    sizes: list[int] = []
    total = 0
    while total < num_rules:
        tier = rng.choices(tiers, weights=weights, k=1)[0]
        size = min(rng.randint(tier.min_size, tier.max_size), num_rules - total)
        sizes.append(size)
        total += size
    return sizes


class RuleLayout:
    """Maps a global rule index to the key ("standard zone") it's nested
    under and its position within that key, given a partition of num_rules
    rules into keys of varying depth (see build_key_sizes)."""

    def __init__(self, key_sizes: list[int]):
        self.key_sizes = key_sizes
        self._prefix = list(itertools.accumulate(key_sizes))

    @property
    def num_keys(self) -> int:
        return len(self.key_sizes)

    def key_for_rule(self, i: int) -> tuple[int, int]:
        """(key_id, position within that key) for global rule index i."""
        key_id = bisect.bisect_right(self._prefix, i)
        offset = self._prefix[key_id - 1] if key_id > 0 else 0
        return key_id, i - offset


def domain_for_key(key_id: int) -> str:
    return f"dragonfly{key_id % NUM_DOMAINS}.com"


def key_fields(key_id: int) -> tuple[int, str, int]:
    """(bucket, domain, port) for a key_id -- the fqdn1 pattern, hash-tag
    apex, and port are all properties of the key, shared by every rule
    nested under it."""
    bucket = key_id % 1000
    domain = domain_for_key(key_id)
    port = _PORTS[key_id % 3]
    return bucket, domain, port


def rule_for_index(i: int, layout: RuleLayout) -> Rule:
    key_id, _ = layout.key_for_rule(i)
    bucket, domain, port = key_fields(key_id)
    return Rule(
        fqdn1=f"*.host{key_id}.corp{bucket}.{domain}",
        port=port,
        fqdn2=f"*.dest{i}.example.com",
        rule_id=f"rule:{i}",
    )


def matching_query_for_index(i: int, layout: RuleLayout) -> Query:
    key_id, _ = layout.key_for_rule(i)
    bucket, domain, port = key_fields(key_id)
    return f"probe.host{key_id}.corp{bucket}.{domain}", port, f"probe.dest{i}.example.com"


def miss_query(rng: random.Random) -> Query:
    # Each miss gets its own apex zone (junk<N>.invalid) rather than one fixed
    # domain shared by every miss query. A shared apex would put every miss's
    # wildcard candidate on the same unbucketed key, hammering it with 100% of
    # miss traffic regardless of concurrency -- the fix mirrors how real
    # apex-tier domains already shard naturally, one key per domain.
    n = rng.randint(0, 2**31 - 1)
    junk_apex = rng.randint(0, 2**31 - 1)
    return f"nomatch{n}.junk{junk_apex}.invalid", 443, f"nomatch{n}.invalid"


def human_bytes(n: int) -> str:
    value = float(n)
    for unit in ("B", "KB", "MB", "GB"):
        if value < 1024.0:
            return f"{int(value)} {unit}" if unit == "B" else f"{value:.2f} {unit}"
        value /= 1024.0
    return f"{value:.2f} TB"


@dataclass(frozen=True, slots=True)
class LatencyStats:
    min: float
    avg: float
    p50: float
    p90: float
    p99: float
    p99_9: float
    max: float


def compute_latency_stats(latencies_ms: list[float]) -> LatencyStats:
    sorted_vals = sorted(latencies_ms)
    n = len(sorted_vals)

    def nearest_rank(p: float) -> float:
        idx = min(n - 1, math.floor(p * n))
        return sorted_vals[idx]

    return LatencyStats(
        min=sorted_vals[0],
        avg=sum(sorted_vals) / n,
        p50=nearest_rank(0.50),
        p90=nearest_rank(0.90),
        p99=nearest_rank(0.99),
        p99_9=nearest_rank(0.999),
        max=sorted_vals[-1],
    )


@dataclass(frozen=True, slots=True)
class SampleResult:
    index: int
    fqdn1: str
    port: int
    fqdn2: str
    result: str
    latency_ms: float


def build_arg_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Benchmark RuleStore FQDN-pair lookups against Redis/Dragonfly."
    )
    parser.add_argument("--num-rules", type=int, default=100_000)
    parser.add_argument("--num-queries", type=int, default=10_000)
    parser.add_argument("--host", type=str, default="localhost")
    parser.add_argument("--port", type=int, default=6379)
    parser.add_argument("--db", type=int, default=0)
    parser.add_argument(
        "--uri",
        type=str,
        default=None,
        help="redis:// or rediss:// URI, an alternative to --host/--port/--db; rediss:// enables TLS automatically",
    )
    parser.add_argument("--mode", choices=("single", "multi"), default="multi")
    parser.add_argument("--miss-rate", type=float, default=0.1)
    parser.add_argument("--write-batch-size", type=int, default=5000)
    parser.add_argument(
        "--query-concurrency",
        type=int,
        default=1,
        help="number of threads issuing queries concurrently (default: 1, sequential)",
    )
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--no-flush", action="store_true")
    parser.add_argument(
        "--query-source",
        choices=("memory", "disk", "reuse"),
        default="memory",
        help="'memory' builds queries directly in memory (default); 'disk' pre-calculates "
        "them, writes them to --query-file, pauses for --query-pause-seconds, then reads "
        "them back in batches to populate the query phase; 'reuse' skips generation "
        "entirely and reads an already-populated --query-file from a prior 'disk' run, "
        "then trims to the first --num-queries of them",
    )
    parser.add_argument(
        "--query-file",
        type=str,
        default="benchmark_queries.tsv",
        help="path used to stage pre-calculated queries when --query-source is disk or reuse",
    )
    parser.add_argument(
        "--query-file-batch-size",
        type=int,
        default=10_000,
        help="batch size for writing/reading --query-file when --query-source is disk or reuse",
    )
    parser.add_argument(
        "--query-pause-seconds",
        type=float,
        default=5.0,
        help="pause, in seconds, after --query-file is fully written and before the query "
        "phase starts, when --query-source disk",
    )
    parser.add_argument(
        "--rule-depth-tiers",
        type=str,
        default=DEFAULT_DEPTH_TIERS,
        help="comma-separated PCT:LO-HI tiers controlling how many rules (endpoints, routes, "
        "or rule exemptions) get nested under one standard-zone key -- e.g. the default "
        f"{DEFAULT_DEPTH_TIERS!r} means 50%% of keys hold 1-3 rules, 30%% hold 4-20, 10%% hold "
        "21-50, and 10%% hold 51-2000. Percentages need not sum to 100 (renormalized).",
    )
    parser.add_argument(
        "--num-buckets",
        type=int,
        default=16,
        help="replicas for the global/tld catch-all tiers (see fqdn_router.RuleStore), so every "
        "lookup's unconditional check of those tiers spreads across this many Dragonfly keys "
        "instead of concentrating on one. Default 16 matches a typical modern core count; size it "
        "to your target server's own thread_count (see fqdn_router.recommended_num_buckets) for "
        "an exact fit. 1 disables bucketing, matching pre-bucketing behavior exactly.",
    )
    return parser


def validate_args(args: argparse.Namespace, parser: argparse.ArgumentParser) -> None:
    if not (1_000 <= args.num_rules <= 500_000_000):
        parser.error(f"--num-rules must be within [1000, 500000000], got {args.num_rules}")
    if not (100 <= args.num_queries <= 50_000_000):
        parser.error(f"--num-queries must be within [100, 50000000], got {args.num_queries}")
    if not (0.0 <= args.miss_rate <= 1.0):
        parser.error(f"--miss-rate must be within [0.0, 1.0], got {args.miss_rate}")
    if not (1 <= args.query_concurrency <= 256):
        parser.error(f"--query-concurrency must be within [1, 256], got {args.query_concurrency}")
    if args.query_file_batch_size < 1:
        parser.error(f"--query-file-batch-size must be >= 1, got {args.query_file_batch_size}")
    if args.query_pause_seconds < 0:
        parser.error(f"--query-pause-seconds must be >= 0, got {args.query_pause_seconds}")
    if args.query_source == "reuse" and not Path(args.query_file).is_file():
        parser.error(f"--query-source reuse requires --query-file to exist, got {args.query_file}")
    if args.num_buckets < 1:
        parser.error(f"--num-buckets must be >= 1, got {args.num_buckets}")
    try:
        parse_depth_tiers(args.rule_depth_tiers)
    except ValueError as exc:
        parser.error(f"--rule-depth-tiers: {exc}")


def print_layout_report(layout: RuleLayout, tiers: list[DepthTier], num_rules: int) -> None:
    print()
    print("=== Key layout ===")
    print(f"standard zones (keys) : {layout.num_keys:,}")
    print(
        f"rules per key         : avg {num_rules / layout.num_keys:.1f}, "
        f"min {min(layout.key_sizes)}, max {max(layout.key_sizes)}"
    )
    for idx, tier in enumerate(tiers, start=1):
        sizes_in_tier = [s for s in layout.key_sizes if tier.min_size <= s <= tier.max_size]
        print(
            f"  tier {idx} [{tier.min_size}-{tier.max_size}] (target {tier.weight * 100:.1f}%): "
            f"{len(sizes_in_tier):,} keys, {sum(sizes_in_tier):,} rules"
        )


def run_write_phase(
    store: RuleStore, num_rules: int, batch_size: int, layout: RuleLayout
) -> tuple[int, int, float]:
    num_batches = math.ceil(num_rules / batch_size)
    progress_interval = max(num_batches // 100, 20)

    total_rules = 0
    total_bytes = 0
    start = time.perf_counter()
    for batch_idx in range(num_batches):
        lo = batch_idx * batch_size
        hi = min(lo + batch_size, num_rules)
        batch = [rule_for_index(i, layout) for i in range(lo, hi)]
        store.put_many(batch)

        total_rules += len(batch)
        total_bytes += sum(len(r.fqdn2.encode()) + len(r.rule_id.encode()) + 1 for r in batch)

        is_last = batch_idx + 1 == num_batches
        if is_last or (batch_idx + 1) % progress_interval == 0:
            elapsed = time.perf_counter() - start
            print(
                f"  write progress: {total_rules:,}/{num_rules:,} rules, "
                f"{human_bytes(total_bytes)} written, {elapsed:.1f}s elapsed"
            )

    elapsed = time.perf_counter() - start
    return total_rules, total_bytes, elapsed


def build_queries(
    num_rules: int, num_queries: int, miss_rate: float, rng: random.Random, layout: RuleLayout
) -> list[Query]:
    num_hits = round(num_queries * (1 - miss_rate))
    num_misses = num_queries - num_hits
    queries = [matching_query_for_index(rng.randrange(num_rules), layout) for _ in range(num_hits)]
    queries += [miss_query(rng) for _ in range(num_misses)]
    rng.shuffle(queries)
    return queries


def write_queries_to_file(queries: list[Query], path: Path, batch_size: int) -> None:
    with open(path, "w") as f:
        for start in range(0, len(queries), batch_size):
            batch = queries[start : start + batch_size]
            f.writelines(f"{fqdn1}\t{port}\t{fqdn2}\n" for fqdn1, port, fqdn2 in batch)


def read_queries_from_file(path: Path, batch_size: int) -> list[Query]:
    queries: list[Query] = []
    batch: list[Query] = []
    with open(path, "r") as f:
        for line in f:
            fqdn1, port_str, fqdn2 = line.rstrip("\n").split("\t")
            batch.append((fqdn1, int(port_str), fqdn2))
            if len(batch) >= batch_size:
                queries.extend(batch)
                batch.clear()
    queries.extend(batch)
    return queries


def warm_up_connections(store: RuleStore, mode: MatchMode, concurrency: int, rng: random.Random) -> None:
    """Open one Redis connection per worker thread ahead of the timed query
    phase. redis.Redis opens sockets lazily, so without this the first task
    on each freshly spawned worker thread would pay its connect (and, for
    rediss://, TLS/auth) cost inside that query's measured latency instead
    of here.

    ThreadPoolExecutor only spawns a new thread when no existing thread is
    idle, so submitting `concurrency` fast lookups doesn't guarantee
    `concurrency` threads: one thread can finish and go idle before the rest
    of the pool has even been spawned, and it then absorbs later tasks
    instead of a new thread being created. A barrier forces every task to
    block until all `concurrency` of them are running, which guarantees one
    thread -- and one warmed connection -- per task.
    """
    if concurrency <= 1:
        return

    barrier = threading.Barrier(concurrency)

    def run_one(query: Query) -> None:
        barrier.wait()
        fqdn1, port, fqdn2 = query
        store.lookup(fqdn1, port, fqdn2, mode)

    warm_queries = [miss_query(rng) for _ in range(concurrency)]
    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        list(pool.map(run_one, warm_queries))


def run_query_phase(
    store: RuleStore, queries: list[Query], mode: MatchMode, concurrency: int = 1
) -> tuple[list[float], list[SampleResult], float]:
    """Run every query and return (per-query latencies in submission order,
    sampled results, wall-clock elapsed for the whole phase).

    concurrency == 1 runs queries one at a time on the calling thread, so
    each latency is a clean serial round trip. concurrency > 1 dispatches
    queries across a thread pool -- redis.Redis draws a pooled connection
    per call, so concurrent store.lookup() calls are safe -- and elapsed
    reflects overlapped wall-clock time, which is what --query-concurrency
    is meant to measure via the queries/sec figure.
    """
    num_queries = len(queries)
    sample_indices = {round(i * (num_queries - 1) / 9) for i in range(10)}

    latencies_ms: list[float] = [0.0] * num_queries
    samples: list[SampleResult] = []
    samples_lock = threading.Lock()

    def run_one(idx: int, query: Query) -> None:
        fqdn1, port, fqdn2 = query
        t0 = time.perf_counter()
        match = store.lookup(fqdn1, port, fqdn2, mode)
        latency_ms = (time.perf_counter() - t0) * 1000.0
        latencies_ms[idx] = latency_ms
        if idx in sample_indices:
            result = match.rule_id if match is not None else "MISS"
            with samples_lock:
                samples.append(SampleResult(idx, fqdn1, port, fqdn2, result, latency_ms))

    start = time.perf_counter()
    if concurrency <= 1:
        for idx, query in enumerate(queries):
            run_one(idx, query)
    else:
        with ThreadPoolExecutor(max_workers=concurrency) as pool:
            list(pool.map(run_one, range(num_queries), queries))
    elapsed = time.perf_counter() - start

    samples.sort(key=lambda s: s.index)
    return latencies_ms, samples, elapsed


def print_write_report(num_rules: int, num_bytes: int, elapsed: float) -> None:
    rate = num_rules / elapsed if elapsed > 0 else float("inf")
    print()
    print("=== Write phase ===")
    print(f"rules written : {num_rules:,}")
    print(f"data written  : {human_bytes(num_bytes)}")
    print(f"elapsed       : {elapsed:.2f}s")
    print(f"rate          : {rate:,.0f} rules/sec")


def print_latency_report(stats: LatencyStats, num_queries: int, elapsed: float, concurrency: int) -> None:
    qps = num_queries / elapsed if elapsed > 0 else float("inf")
    print()
    print("=== Query latency (ms) ===")
    print(f"{'min':>8} {'avg':>8} {'p50':>8} {'p90':>8} {'p99':>8} {'p99.9':>8} {'max':>8}")
    print(
        f"{stats.min:8.3f} {stats.avg:8.3f} {stats.p50:8.3f} {stats.p90:8.3f} "
        f"{stats.p99:8.3f} {stats.p99_9:8.3f} {stats.max:8.3f}"
    )
    print()
    print("=== Query throughput ===")
    print(f"queries       : {num_queries:,}")
    print(f"concurrency   : {concurrency}")
    print(f"elapsed       : {elapsed:.2f}s")
    print(f"rate          : {qps:,.0f} queries/sec")


def print_sample_report(samples: list[SampleResult]) -> None:
    print()
    print("=== Sample results (10 evenly-spaced queries) ===")
    print(f"{'idx':>6}  {'fqdn1':<45} {'port':>5} {'fqdn2':<30} {'result':<20} {'latency_ms':>10}")
    for s in samples:
        print(f"{s.index:6d}  {s.fqdn1:<45} {s.port:5d} {s.fqdn2:<30} {s.result:<20} {s.latency_ms:10.3f}")


def describe_target(args: argparse.Namespace) -> str:
    if not args.uri:
        return f"{args.host}:{args.port} db={args.db}"
    parts = urllib.parse.urlsplit(args.uri)
    if parts.password is None:
        return args.uri
    userinfo, _, hostport = parts.netloc.rpartition("@")
    user, _, _ = userinfo.partition(":")
    netloc = f"{user}:***@{hostport}"
    return urllib.parse.urlunsplit((parts.scheme, netloc, parts.path, parts.query, parts.fragment))


def main() -> None:
    parser = build_arg_parser()
    args = parser.parse_args()
    validate_args(args, parser)

    mode = MatchMode.SINGLE_LEVEL if args.mode == "single" else MatchMode.MULTI_LEVEL

    if args.uri:
        conn = redis.Redis.from_url(args.uri, max_connections=64)
    else:
        conn = redis.Redis(host=args.host, port=args.port, db=args.db)
    try:
        conn.ping()
    except redis.exceptions.RedisError as exc:
        print(f"error: cannot reach redis at {describe_target(args)}: {exc}", file=sys.stderr)
        sys.exit(1)

    try:
        store = RuleStore(conn, namespace="bench", num_buckets=args.num_buckets)
    except ValueError as exc:
        print(f"error: {exc}", file=sys.stderr)
        sys.exit(1)
    rng = random.Random(args.seed)

    needs_layout = not args.no_flush or args.query_source != "reuse"
    layout: RuleLayout | None = None
    if needs_layout:
        tiers = parse_depth_tiers(args.rule_depth_tiers)
        # Deliberately independent of --seed (see LAYOUT_SEED) so a --no-flush
        # rerun with a different --seed still resamples queries against the
        # same previously-written keys, matching the pre-existing invariant
        # that --seed alone controls query selection, not what got written.
        layout_rng = random.Random(LAYOUT_SEED)
        layout = RuleLayout(build_key_sizes(args.num_rules, tiers, layout_rng))
        print_layout_report(layout, tiers, args.num_rules)

    if not args.no_flush:
        print(f"warning: flushing {describe_target(args)} before writing")
        conn.flushdb()
        print(f"writing {args.num_rules:,} rules in batches of {args.write_batch_size:,}...")
        num_written, num_bytes, write_elapsed = run_write_phase(
            store, args.num_rules, args.write_batch_size, layout
        )
        print_write_report(num_written, num_bytes, write_elapsed)

    if args.query_source == "reuse":
        query_path = Path(args.query_file)
        print(f"\nreusing pre-written queries from {query_path} in batches of {args.query_file_batch_size:,}...")
        queries = read_queries_from_file(query_path, args.query_file_batch_size)
        if len(queries) < args.num_queries:
            print(
                f"error: --query-file {query_path} has {len(queries):,} queries, "
                f"fewer than --num-queries {args.num_queries:,}",
                file=sys.stderr,
            )
            sys.exit(1)
        queries = queries[: args.num_queries]
    else:
        queries = build_queries(args.num_rules, args.num_queries, args.miss_rate, rng, layout)
        if args.query_source == "disk":
            query_path = Path(args.query_file)
            print(
                f"\nwriting {len(queries):,} pre-calculated queries to {query_path} "
                f"in batches of {args.query_file_batch_size:,}..."
            )
            write_queries_to_file(queries, query_path, args.query_file_batch_size)
            print(f"pausing {args.query_pause_seconds:.1f}s before the query phase...")
            time.sleep(args.query_pause_seconds)
            print(f"reading queries back from {query_path} in batches of {args.query_file_batch_size:,}...")
            queries = read_queries_from_file(query_path, args.query_file_batch_size)

    if args.query_concurrency > 1:
        print(f"\nwarming up {args.query_concurrency} connection(s)...")
        warm_up_connections(store, mode, args.query_concurrency, rng)

    print(
        f"\nrunning {len(queries):,} queries "
        f"(mode={mode.value}, concurrency={args.query_concurrency})..."
    )
    latencies_ms, samples, query_elapsed = run_query_phase(store, queries, mode, args.query_concurrency)
    stats = compute_latency_stats(latencies_ms)
    print_latency_report(stats, len(queries), query_elapsed, args.query_concurrency)
    print_sample_report(samples)


if __name__ == "__main__":
    main()
