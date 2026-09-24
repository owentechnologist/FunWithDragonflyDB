# /// script
# requires-python = ">=3.9"
# dependencies = ["redis"]
# ///
"""Standalone diagnostic: does hammering ONE always-the-same key concentrate
Dragonfly's CPU onto a single proactor thread, versus hammering many
distinct hash-tagged keys spreading it across threads -- regardless of how
many client connections/threads generate the load?

Context: fqdn_router.py's RuleStore checks a global catch-all tier ("*" ->
hash tag "__global__") on every single lookup, and (under MULTI_LEVEL mode)
a per-TLD tier too (e.g. "__tld__:com") -- both map to one fixed Dragonfly
key/shard no matter which domain is being looked up, while the well-spread
apex tier maps to a different shard per domain. A prior diagnostic
(diagnose_thread_pinning.py) ruled out client connection placement as the
bottleneck on this server -- connections spread evenly across all proactor
threads regardless of burst/stagger or shared/independent pool. This script
tests the remaining hypothesis directly, isolated from any router/Lua-script
complexity: plain INCR against one fixed key vs. INCR spread across many
distinct {tag}-sharded keys.

Method: for each of two load phases -- "concentrated" (every command
touches the same literal key) and "distributed" (each command touches a
different key, spread across --num-distinct-tags via Redis hash-tag syntax)
-- drive sustained INCR load from --concurrency threads (each its own
connection) for --duration-seconds, sampling per-thread CPU ticks from
/proc/<dragonfly_pid>/task/*/stat before and after. Reports which thread(s)
absorbed the CPU in each phase.

Requires running where /proc/<dragonfly_pid> is visible (i.e. on the
Dragonfly host itself, or inside its container/PID namespace) to get
automatic measurement. If that's not available, it still runs the load and
prints the redis-cli/top commands to compare by hand.

Usage:
    uv run diagnose_hot_tag_routing.py --host localhost --port 6379
    uv run diagnose_hot_tag_routing.py --uri rediss://default:pw@host:6385 --pid 4821
"""
from __future__ import annotations

import argparse
import os
import time
from concurrent.futures import ThreadPoolExecutor

import redis

try:
    CLK_TCK = os.sysconf("SC_CLK_TCK")
except (AttributeError, ValueError):
    CLK_TCK = 100.0


def read_thread_cpu_ticks(pid: int) -> dict[int, int]:
    """utime+stime, in clock ticks, per thread of `pid`, from
    /proc/<pid>/task/<tid>/stat. Field layout: after the comm field (which
    may itself contain spaces/parens, hence rfind(')')), the remaining
    space-separated fields start at "state" (field 3); utime is field 14,
    stime is field 15, i.e. rest[11] and rest[12] 0-indexed from "state".
    """
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


def generate_load(
    make_client, concurrency: int, duration_seconds: float, key_fn, pipeline_size: int = 1
) -> int:
    """pipeline_size > 1 batches that many INCRs into one pipelined round
    trip, to find a thread's true saturation point uncontaminated by
    client-side per-call Python/GIL overhead -- see the module docstring's
    note on distinguishing "client can't push enough ops/sec" from "the
    thread is genuinely at its ceiling"."""
    stop_at = time.monotonic() + duration_seconds
    counts = [0] * concurrency

    def worker(idx: int) -> None:
        client = make_client()
        try:
            n = 0
            if pipeline_size <= 1:
                while time.monotonic() < stop_at:
                    client.incr(key_fn(n))
                    n += 1
            else:
                while time.monotonic() < stop_at:
                    pipe = client.pipeline(transaction=False)
                    for _ in range(pipeline_size):
                        pipe.incr(key_fn(n))
                        n += 1
                    pipe.execute()
            counts[idx] = n
        finally:
            client.close()

    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        list(pool.map(worker, range(concurrency)))
    return sum(counts)


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


def build_arg_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--host", default="localhost")
    parser.add_argument("--port", type=int, default=6379)
    parser.add_argument("--db", type=int, default=0)
    parser.add_argument("--uri", type=str, default=None, help="redis:// or rediss:// URI instead of host/port/db")
    parser.add_argument("--pid", type=int, default=None, help="dragonfly PID; auto-detected via INFO if omitted")
    parser.add_argument("--concurrency", type=int, default=16)
    parser.add_argument("--duration-seconds", type=float, default=5.0)
    parser.add_argument("--num-distinct-tags", type=int, default=2000)
    parser.add_argument("--namespace", type=str, default="hottagdiag")
    parser.add_argument(
        "--pipeline-size",
        type=int,
        default=1,
        help="batch this many INCRs per round trip, to find a thread's true saturation point "
        "uncontaminated by client-side per-call overhead (1 = no pipelining, one INCR per round trip)",
    )
    parser.add_argument(
        "--phases",
        type=str,
        default="concentrated,distributed",
        help="comma-separated subset of concentrated,distributed",
    )
    return parser


def main() -> None:
    args = build_arg_parser().parse_args()

    def make_client() -> redis.Redis:
        if args.uri:
            return redis.Redis.from_url(args.uri)
        return redis.Redis(host=args.host, port=args.port, db=args.db)

    observer = make_client()
    server_info = observer.info("server")
    pid = args.pid or server_info.get("process_id")
    print(
        f"dragonfly_version={server_info.get('dragonfly_version', 'n/a')} "
        f"thread_count={server_info.get('thread_count', 'n/a')} pid={pid}"
    )

    can_read_proc = False
    if pid:
        try:
            os.listdir(f"/proc/{pid}/task")
            can_read_proc = True
        except OSError as exc:
            print(f"warning: cannot read /proc/{pid}/task ({exc}) -- falling back to manual instructions")
    else:
        print("warning: could not determine PID (pass --pid; INFO has no process_id)")

    hot_key = f"probe:{{{args.namespace}:hot}}"

    def hot_key_fn(_n: int) -> str:
        return hot_key

    def spread_key_fn(n: int) -> str:
        return f"probe:{{{args.namespace}:spread{n % args.num_distinct_tags}}}"

    all_phases = {
        "concentrated": ("concentrated (one fixed key)", hot_key_fn),
        "distributed": ("distributed (many distinct tags)", spread_key_fn),
    }
    requested = [p.strip() for p in args.phases.split(",") if p.strip()]
    phases = [all_phases[p] for p in requested]

    for label, key_fn in phases:
        print(
            f"\n=== {label}: concurrency={args.concurrency} duration={args.duration_seconds}s "
            f"pipeline_size={args.pipeline_size} ==="
        )
        before = read_thread_cpu_ticks(pid) if can_read_proc else None
        if not can_read_proc:
            print(f"  (manual) before: top -bH -n 1 -p {pid or '<PID>'}")
        total_ops = generate_load(
            make_client, args.concurrency, args.duration_seconds, key_fn, args.pipeline_size
        )
        print(f"  {total_ops:,} ops issued ({total_ops / args.duration_seconds:,.0f} ops/sec)")
        if can_read_proc:
            after = read_thread_cpu_ticks(pid)
            print_thread_report(before, after, args.duration_seconds)
        else:
            print(f"  (manual) after:  top -bH -n 1 -p {pid or '<PID>'}  -- compare TIME+/%CPU per thread to before")


if __name__ == "__main__":
    main()
