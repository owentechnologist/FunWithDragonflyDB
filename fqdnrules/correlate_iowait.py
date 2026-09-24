# /// script
# requires-python = ">=3.9"
# dependencies = []
# ///
"""Run go-benchmark while sampling `iostat` in the background, then correlate
each row of the benchmark's own "=== Top N slowest queries ===" report
against the iostat sample closest to that query's timestamp.

Why this script exists: a `top -H` snapshot taken during a benchmark run
showed 56.8% %iowait system-wide, with 0% steal and 0% swap in use, on a
workload that should be pure in-memory (persistence confirmed off). That's
a plausible explanation for the one thing this whole investigation never
pinned down -- a rare ~20-30ms max-latency outlier that showed up
identically regardless of concurrency, dataset shape, or code path (direct
EVALSHA vs routerd/gRPC). A load-independent, occasional stall is exactly
the signature of disk I/O wait (EBS burst-credit throttling, a background
block-layer hiccup) rather than anything in Dragonfly, routerd, or the
benchmark's own logic. This script makes that testable: it lines up the
benchmark's own top-10-slowest rows (added specifically so they carry
millisecond timestamps) against iostat's per-second %iowait/%util/await, so
a specific slow query can be checked against what the disk was doing at
that exact moment.

Usage (run this ON the Dragonfly host, right where you'd normally invoke
./benchmark -- everything after `--` is passed straight through to it):

    uv run correlate_iowait.py --benchmark ./benchmark -- \\
        -uri redis://localhost:6385 -num-rules 10000000 --no-flush \\
        -target grpc=localhost:9090 -num-buckets 32 -query-concurrency 32 \\
        -mode single -num-queries 2130000

Or without uv: `python3 correlate_iowait.py --benchmark ./benchmark -- ...`

Requires `iostat` (the sysstat package -- `sudo apt install sysstat` on
Ubuntu if it's missing) and a benchmark binary built with the top-10-slowest
report. No other dependencies: stdlib only, safe to copy to any machine on
its own.
"""
from __future__ import annotations

import argparse
import os
import re
import shutil
import subprocess
import sys
import time
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from pathlib import Path

TOP_SLOW_HEADER_RE = re.compile(r"^=== Top \d+ slowest queries")
TOP_SLOW_ROW_RE = re.compile(
    r"^\s*(?P<rank>\d+)\.\s+(?P<ts>\S+)\s+(?P<lat>[\d.]+)ms\s+(?P<op>.+?)\s+(?P<detail>fqdn1=.*)$"
)
# Matches both go-benchmark's "-05:00"-style offsets and iostat's ISO
# "+0000"-style offsets (set via S_TIME_FORMAT=ISO below), so both logs
# parse into one comparable, timezone-aware datetime type.
ISO_RE = re.compile(
    r"^(?P<base>\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?)(?P<sign>[+-])(?P<oh>\d{2}):?(?P<om>\d{2})$"
)


def parse_iso(ts: str) -> datetime:
    m = ISO_RE.match(ts.strip())
    if not m:
        return datetime.fromisoformat(ts.strip())
    base = datetime.fromisoformat(m["base"])
    delta = timedelta(hours=int(m["oh"]), minutes=int(m["om"]))
    if m["sign"] == "-":
        delta = -delta
    return base.replace(tzinfo=timezone(delta))


@dataclass
class SlowQuery:
    rank: int
    timestamp: datetime
    latency_ms: float
    op: str
    detail: str


@dataclass
class IostatSample:
    timestamp: datetime
    iowait_pct: float
    devices: dict = field(default_factory=dict)  # name -> (util_pct, await_ms)


def parse_top_slow(benchmark_log: str) -> list[SlowQuery]:
    out: list[SlowQuery] = []
    in_section = False
    for line in benchmark_log.splitlines():
        if TOP_SLOW_HEADER_RE.match(line):
            in_section = True
            continue
        if not in_section:
            continue
        if line.startswith("===") and not TOP_SLOW_HEADER_RE.match(line):
            break
        stripped = line.strip()
        if stripped == "" or stripped.startswith("keys:") or stripped.startswith("("):
            continue
        m = TOP_SLOW_ROW_RE.match(line)
        if not m:
            continue
        out.append(
            SlowQuery(
                rank=int(m["rank"]),
                timestamp=parse_iso(m["ts"]),
                latency_ms=float(m["lat"]),
                op=m["op"],
                detail=m["detail"].strip(),
            )
        )
    return out


def parse_iostat(iostat_log: str) -> list[IostatSample]:
    samples: list[IostatSample] = []
    current: IostatSample | None = None
    mode: str | None = None  # None | "cpu-header" | "dev-rows"
    dev_fields: list[str] = []
    for raw in iostat_log.splitlines():
        line = raw.rstrip()
        if not line:
            continue
        try:
            ts = parse_iso(line)
        except ValueError:
            ts = None
        if ts is not None:
            current = IostatSample(timestamp=ts, iowait_pct=0.0)
            samples.append(current)
            mode = None
            continue
        if current is None:
            continue
        if line.startswith("avg-cpu:"):
            mode = "cpu-header"
            continue
        if mode == "cpu-header":
            parts = line.split()
            # iostat's avg-cpu column order is always:
            # %user %nice %system %iowait %steal %idle
            try:
                current.iowait_pct = float(parts[3])
            except (IndexError, ValueError):
                pass
            mode = None
            continue
        if line.startswith("Device"):
            dev_fields = line.split()
            mode = "dev-rows"
            continue
        if mode == "dev-rows":
            parts = line.split()
            if len(parts) < 2:
                continue
            name = parts[0]
            field_map = dict(zip(dev_fields, parts))
            util = field_map.get("%util")
            await_ = field_map.get("await") or field_map.get("r_await")
            try:
                util_f = float(util) if util is not None else 0.0
            except ValueError:
                util_f = 0.0
            try:
                await_f = float(await_) if await_ is not None else 0.0
            except ValueError:
                await_f = 0.0
            current.devices[name] = (util_f, await_f)
    # iostat's first report is cumulative since boot, not a real interval
    # sample -- meaningless for correlating against a specific moment.
    return samples[1:] if len(samples) > 1 else samples


def closest_sample(samples: list[IostatSample], ts: datetime) -> IostatSample | None:
    if not samples:
        return None
    return min(samples, key=lambda s: abs((s.timestamp - ts).total_seconds()))


def build_arg_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--benchmark", default="./benchmark", help="path to the go-benchmark binary")
    parser.add_argument("--interval", type=int, default=1, help="iostat sampling interval in seconds")
    parser.add_argument(
        "--out-dir", default=None, help="where to write benchmark.log/iostat.log (default: a fresh /tmp dir)"
    )
    parser.add_argument(
        "--iowait-alert-pct",
        type=float,
        default=10.0,
        help="flag a slow query as iowait-correlated if the nearest sample's %%iowait is at or above this",
    )
    parser.add_argument("args", nargs=argparse.REMAINDER, help="-- followed by the benchmark's own flags")
    return parser


def main() -> None:
    ns = build_arg_parser().parse_args()

    bench_args = ns.args
    if bench_args and bench_args[0] == "--":
        bench_args = bench_args[1:]
    if not bench_args:
        build_arg_parser().error("pass the benchmark's own flags after --, e.g. -- -uri redis://localhost:6385 ...")

    if shutil.which("iostat") is None:
        build_arg_parser().error(
            "iostat not found on PATH -- install the sysstat package (e.g. `sudo apt install sysstat`)"
        )

    out_dir = Path(ns.out_dir) if ns.out_dir else Path(f"/tmp/bench-iowait-{int(time.time())}")
    out_dir.mkdir(parents=True, exist_ok=True)
    bench_log_path = out_dir / "benchmark.log"
    iostat_log_path = out_dir / "iostat.log"
    print(f"logging to {out_dir}/")

    iostat_env = {**os.environ, "S_TIME_FORMAT": "ISO"}
    iostat_proc = subprocess.Popen(
        ["iostat", "-xmt", str(ns.interval)],
        stdout=open(iostat_log_path, "w"),
        stderr=subprocess.STDOUT,
        env=iostat_env,
    )

    bench_cmd = [ns.benchmark, *bench_args]
    print(f"running: {' '.join(bench_cmd)}")
    bench_lines: list[str] = []
    proc = subprocess.Popen(bench_cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, bufsize=1)
    with open(bench_log_path, "w") as bench_log_file:
        for line in proc.stdout:
            sys.stdout.write(line)
            bench_log_file.write(line)
            bench_lines.append(line.rstrip("\n"))
        proc.wait()

    iostat_proc.terminate()
    try:
        iostat_proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        iostat_proc.kill()

    if proc.returncode != 0:
        print(f"\nwarning: benchmark exited with code {proc.returncode}; correlating whatever it printed anyway",
              file=sys.stderr)

    slow_queries = parse_top_slow("\n".join(bench_lines))
    iostat_samples = parse_iostat(iostat_log_path.read_text())

    if not slow_queries:
        print(
            "\nno '=== Top N slowest queries ===' rows found -- is this a benchmark build with the top-slow "
            "report, and did any query clear -slow-latency-gate (default 0, so this shouldn't normally happen)?"
        )
        return
    if not iostat_samples:
        print(f"\nno iostat samples parsed -- check {iostat_log_path}")
        return

    all_iowait = sorted(s.iowait_pct for s in iostat_samples)
    baseline = all_iowait[len(all_iowait) // 2]  # median: robust to the very spikes being hunted for

    print(f"\n=== iowait correlation (n={len(iostat_samples)} samples, median %iowait over the run: {baseline:.1f}) ===")
    for q in slow_queries:
        sample = closest_sample(iostat_samples, q.timestamp)
        print(f"{q.rank:2d}. {q.timestamp.isoformat()}  {q.latency_ms:9.3f}ms  {q.op:<10} {q.detail}")
        if sample is None:
            print("      no iostat sample available")
            continue
        gap = abs((sample.timestamp - q.timestamp).total_seconds())
        flag = "  <-- ELEVATED IOWAIT" if sample.iowait_pct >= ns.iowait_alert_pct else ""
        print(f"      nearest iostat sample @ {sample.timestamp.isoformat()} (Δ{gap:.1f}s): "
              f"%iowait={sample.iowait_pct:.1f}{flag}")
        top_devices = sorted(sample.devices.items(), key=lambda kv: -kv[1][0])[:3]
        if top_devices:
            dev_str = ", ".join(f"{name} util={util:.1f}% await={await_:.1f}ms" for name, (util, await_) in top_devices)
            print(f"      devices: {dev_str}")


if __name__ == "__main__":
    main()
