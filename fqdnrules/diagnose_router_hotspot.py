# /// script
# requires-python = ">=3.9"
# dependencies = ["redis"]
# ///
"""Standalone diagnostic: does a REAL fqdn_router.RuleStore.lookup() workload
(the exact same Lua script and 3-group-per-lookup shape benchmark.py uses)
concentrate CPU onto one Dragonfly proactor thread, the way the raw-INCR
proxy in diagnose_hot_tag_routing.py suggested but couldn't confirm?

Why this script exists: diagnose_hot_tag_routing.py showed that a raw INCR
against one fixed key does concentrate CPU on that key's owning thread, but
pipelining the same INCRs 150-deep dropped that thread's absolute CPU time
rather than raising it -- proof that most of what was measured was
per-command protocol/dispatch overhead, not real work, and that a bare
INCR is a poor stand-in for what the router's Lua script (looping HMGET
over several candidate fields per group) actually costs. This script drops
the proxy and measures the real thing directly.

Setup: writes `--num-domains` distinct apex-tier rules (spreading that tier
across many keys/shards, like a real dataset) plus one rule under the
literal "*" global pattern, all under a private `--namespace` so it never
touches or needs to flush your real benchmark data. Then `--concurrency`
threads repeatedly call the real `RuleStore.lookup()` against a rotating
set of those domains -- every single call still issues the same up-to-3
grouped EVALSHAs a real lookup always does (apex + tld + global), exactly
as benchmark.py's query phase does, just with per-thread CPU sampled
around it via /proc/<dragonfly_pid>/task/*/stat.

Requires fqdn_router.py and fqdn_pattern.py in the same directory as this
file (copy all three together to another machine) -- it imports RuleStore
directly rather than reimplementing the Lua script, so what's measured
here is guaranteed to be the actual production cost, not a redescription
of it that could silently drift out of sync.

Requires running where /proc/<dragonfly_pid> is visible (the Dragonfly
host itself, or inside its container/PID namespace) for automatic
measurement; otherwise it still runs the load and prints the `top -bH`
commands to compare by hand.

Usage:
    uv run diagnose_router_hotspot.py --host localhost --port 6379
    uv run diagnose_router_hotspot.py --uri rediss://default:pw@host:6385 \\
        --concurrency 32 --num-domains 5000 --mode multi
"""
from __future__ import annotations

import argparse
import os
import sys
import time
from concurrent.futures import ThreadPoolExecutor

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import redis

from fqdn_pattern import MatchMode
from fqdn_router import Rule, RuleStore, recommended_num_buckets

try:
    CLK_TCK = os.sysconf("SC_CLK_TCK")
except (AttributeError, ValueError):
    CLK_TCK = 100.0


def read_thread_cpu_ticks(pid: int) -> dict[int, int]:
    ticks: dict[int, int] = {}
    task_dir = f"/proc/{pid}/task"
    for tid_str in os.listdir(task_dir):
        try:
            with open(f"{task_dir}/{tid_str}/stat") as f:
                data = f.read()
        except OSError:
            continue
        rest = data[data.rfind(")") + 2 :].split()
        ticks[int(tid_str)] = int(rest[11]) + int(rest[12])
    return ticks


def print_thread_report(before: dict[int, int], after: dict[int, int], duration: float) -> None:
    deltas = {tid: after.get(tid, 0) - before.get(tid, 0) for tid in after}
    deltas = {tid: d for tid, d in deltas.items() if d > 0}
    if not deltas:
        print("  no per-thread CPU delta observed (load may have been too light/short)")
        return
    total = sum(deltas.values())
    ranked = sorted(deltas.items(), key=lambda kv: -kv[1])
    print(f"  total measured CPU: {total / CLK_TCK:.2f}s (core-seconds) over {duration:.1f}s wall")
    for tid, d in ranked[:6]:
        pct_of_total = 100 * d / total
        pct_of_wall = 100 * (d / CLK_TCK) / duration
        print(
            f"    tid={tid}: {d / CLK_TCK:.2f}s -- {pct_of_total:.0f}% of measured CPU, "
            f"{pct_of_wall:.0f}% of one core's wall time"
        )
    top_share = 100 * ranked[0][1] / total
    if top_share > 60:
        verdict = "CONCENTRATED"
    elif top_share > 30:
        verdict = "PARTIALLY CONCENTRATED"
    else:
        verdict = "SPREAD"
    print(f"  verdict: {verdict} -- busiest thread absorbed {top_share:.0f}% of measured CPU")


def setup_rules(store: RuleStore, num_domains: int, port: int) -> None:
    rules = [
        Rule(
            fqdn1=f"*.svc.hotspotdiag{i}.com",
            port=port,
            fqdn2="*.dest.example.com",
            rule_id=f"rule:{i}",
        )
        for i in range(num_domains)
    ]
    rules.append(Rule(fqdn1="*", port=port, fqdn2="*", rule_id="rule:global"))
    store.put_many(rules)


def run_lookups(
    make_store, num_domains: int, port: int, mode: MatchMode, concurrency: int, duration_seconds: float
) -> int:
    stop_at = time.monotonic() + duration_seconds
    counts = [0] * concurrency

    def worker(idx: int) -> None:
        store = make_store()
        n = 0
        while time.monotonic() < stop_at:
            i = (idx * 7919 + n) % num_domains  # spread each thread's rotation, avoid lockstep
            fqdn1 = f"probe.svc.hotspotdiag{i}.com"
            fqdn2 = "probe.dest.example.com"
            store.lookup(fqdn1, port, fqdn2, mode)
            n += 1
        counts[idx] = n

    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        list(pool.map(worker, range(concurrency)))
    return sum(counts)


def build_arg_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--host", default="localhost")
    parser.add_argument("--port", type=int, default=6379)
    parser.add_argument("--db", type=int, default=0)
    parser.add_argument("--uri", type=str, default=None, help="redis:// or rediss:// URI instead of host/port/db")
    parser.add_argument("--pid", type=int, default=None, help="dragonfly PID; auto-detected via INFO if omitted")
    parser.add_argument("--namespace", type=str, default="hotspotdiag")
    parser.add_argument("--num-domains", type=int, default=2000)
    parser.add_argument("--rule-port", type=int, default=443, help="port the synthetic rules/queries use")
    parser.add_argument("--mode", choices=("single", "multi"), default="multi")
    parser.add_argument("--concurrency", type=int, default=16)
    parser.add_argument("--duration-seconds", type=float, default=5.0)
    parser.add_argument(
        "--num-buckets",
        type=str,
        default="1",
        help="replicas for the global/tld catch-all tiers: an integer (e.g. 32), or 'auto' to size "
        "from the server's own reported thread_count via recommended_num_buckets()",
    )
    return parser


def main() -> None:
    args = build_arg_parser().parse_args()
    mode = MatchMode.SINGLE_LEVEL if args.mode == "single" else MatchMode.MULTI_LEVEL

    def make_conn() -> redis.Redis:
        if args.uri:
            return redis.Redis.from_url(args.uri)
        return redis.Redis(host=args.host, port=args.port, db=args.db)

    observer_conn = make_conn()
    server_info = observer_conn.info("server")
    pid = args.pid or server_info.get("process_id")
    print(
        f"dragonfly_version={server_info.get('dragonfly_version', 'n/a')} "
        f"thread_count={server_info.get('thread_count', 'n/a')} pid={pid}"
    )

    if args.num_buckets == "auto":
        num_buckets = recommended_num_buckets(observer_conn)
        print(f"num_buckets=auto -> {num_buckets} (from server thread_count)")
    else:
        num_buckets = int(args.num_buckets)
        print(f"num_buckets={num_buckets}")

    def make_store() -> RuleStore:
        return RuleStore(make_conn(), namespace=args.namespace, num_buckets=num_buckets)

    can_read_proc = False
    if pid:
        try:
            os.listdir(f"/proc/{pid}/task")
            can_read_proc = True
        except OSError as exc:
            print(f"warning: cannot read /proc/{pid}/task ({exc}) -- falling back to manual instructions")
    else:
        print("warning: could not determine PID (pass --pid; INFO has no process_id)")

    print(f"writing {args.num_domains:,} apex rules + 1 global rule under namespace={args.namespace!r}...")
    setup_store = make_store()
    setup_rules(setup_store, args.num_domains, args.rule_port)

    print(
        f"\n=== router lookups: mode={mode.value} concurrency={args.concurrency} "
        f"duration={args.duration_seconds}s domains={args.num_domains:,} ==="
    )
    before = read_thread_cpu_ticks(pid) if can_read_proc else None
    if not can_read_proc:
        print(f"  (manual) before: top -bH -n 1 -p {pid or '<PID>'}")

    total_queries = run_lookups(
        make_store, args.num_domains, args.rule_port, mode, args.concurrency, args.duration_seconds
    )
    print(f"  {total_queries:,} lookups issued ({total_queries / args.duration_seconds:,.0f} queries/sec)")

    if can_read_proc:
        after = read_thread_cpu_ticks(pid)
        print_thread_report(before, after, args.duration_seconds)
    else:
        print(f"  (manual) after:  top -bH -n 1 -p {pid or '<PID>'}  -- compare TIME+/%CPU per thread to before")


if __name__ == "__main__":
    main()
