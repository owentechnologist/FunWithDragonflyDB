# How to run

Requires a Redis or Dragonfly instance reachable at `localhost:6379`
(tests use `EVALSHA`, so it can't be faked with a mock).

## go version comes in two flavors:
* smart client 'benchmark' that can write the dataset and does all calculations and calls LUA script server side directly

- from the go-benchmark directory:
You need to populate the target Redis for example with 10 million rules:

```
./benchmark -uri rediss://myredis.com:6379 -num-rules 10000000 -num-buckets 32
```

The nature of the queries I model is as follows:  

```
~/go-benchmark$ $HOME/go/bin/grpcurl -plaintext -import-path ../go-router-service/proto -proto router.proto -d '{"fqdn1": "probe.host7.corp3.dragonfly42.com", "port": 443, "fqdn2": 

  "dev.probe.dest42.example.com", "mode": "SINGLE_LEVEL"}' localhost:9090 router.Router/Lookup

{}
```

```
~/go-benchmark$ $HOME/go/bin/grpcurl -plaintext -import-path ../go-router-service/proto -proto router.proto -d '{"fqdn1": "probe.host7.corp3.dragonfly42.com", "port": 443, "fqdn2": 

  "dev.probe.dest42.example.com", "mode": "MULTI_LEVEL"}' localhost:9090 router.Router/Lookup

{
  "hit": true,
  "ruleId": "rule:42",
  "fqdn1Pattern": "*",
  "fqdn2Pattern": "*.dest42.example.com",
  "rank1": 255,
  "rank2": 2
}
```

This check on single level/exact-rule matches is much faster than the multi-level. 
If you need multi-level that needs to call multiple keys in the cases where exact match isn't available.

- to run a full benchmark, rather than a single query, do the following:

```
./benchmark -uri rediss://master.my-cache-cluster.iuo1te.use1.cache.amazonaws.com:6379 -num-rules 10000000 -num-buckets 32 -query-concurrency 125 -num-queries 1500000
```

* dumb client that calls a proxy layer 'routerd' with the smarts using gorpc

- from the go-router-service directory:

```
 ./routerd -uri rediss://master.my-cache-cluster.iuo1te.use1.cache.amazonaws.com:6379 -num-buckets 32 -pool-size 250
```
- and then from the go-benchmark directory:

```
./benchmark -no-flush -target grpc=localhost:9090 -query-concurrency 250 -uri rediss://master.my-cache-cluster.iuo1te.use1.cache.amazonaws.com:6379 -num-buckets 32 -num-queries 1500000
```

# python version is not as scalable and fails to drive enough load to get to 100K ops/second from a single box:

```sh
uv run pytest test_fqdn_router.py
```

Tests run against db 15 and flush it before and after each test, so they
never touch real data.

## Benchmark (benchmark.py)

Loads a batch of synthetic `(fqdn1, port, fqdn2)` rules into `RuleStore`
(`fqdn_router.py`), then runs a batch of timed lookups against them and
reports write throughput plus lookup latency percentiles.

`hiredis` is a project dependency (`pyproject.toml`), which redis-py detects
automatically and switches its RESP parser to at import time -- no code
here references it directly. It moves response parsing from pure Python
into C, cutting client-side per-call CPU cost; worth knowing about since it
affects how much throughput a single client process can generate before
becoming the bottleneck itself, separate from anything server-side.

```sh
uv run benchmark.py --num-rules 100000 --num-queries 10000
```

Key flags (all optional, see `--help` for the rest): `--host`/`--port`/`--db`
to point at a different instance, `--uri` to pass a `redis://` or
`rediss://` URI instead (`rediss://` turns on TLS, and
`--host`/`--port`/`--db` are ignored), `--mode single|multi` to pick the
matching mode (default `multi`), `--miss-rate` to control what fraction of
queries are guaranteed non-matches (default `0.1`), `--write-batch-size` to
size the pipelined write batches, `--rule-depth-tiers` to control how many
rules get nested under one key (see below), `--num-buckets` to control how
many replicas the global/tld catch-all tiers spread across (default `16`;
see below), `--seed` for reproducible key layout and query selection,
`--query-concurrency` to run the query phase from multiple threads at once
(default `1`, sequential; see below), `--no-flush` to skip the `FLUSHDB` the
benchmark otherwise runs against the target db before writing (default db
is 15, not a real-data db, but still flushed unless you pass this), and
`--query-source disk`/`reuse` to stage queries through a local file instead
of keeping them in memory, or to skip generation entirely and reuse a file
from a prior run (see below).

### What it does

1. **Write phase** — partitions `--num-rules` rules across keys ("standard
   zones") per `--rule-depth-tiers`, generates the resulting synthetic
   `Rule`s, and writes them via `RuleStore.put_many`, pipelined in batches
   of `--write-batch-size`. Reports rules/sec and bytes written.
2. **Query phase** — builds `--num-queries` lookups: `(1 - miss_rate)` of
   them target a rule written in phase 1 (guaranteed hit), the rest are
   randomly generated FQDNs guaranteed not to match anything (guaranteed
   miss). The combined list is shuffled using `--seed`. With the default
   `--query-source memory` the shuffled list goes straight into the timed
   run; with `--query-source disk` it's written to `--query-file` first,
   the benchmark pauses for `--query-pause-seconds`, then reads the file
   back in batches of `--query-file-batch-size` to repopulate the same
   list; with `--query-source reuse` generation is skipped entirely and
   the list is populated straight from an existing `--query-file` written
   by an earlier `disk` run (see below). Either way, each query is then run through
   `RuleStore.lookup` with per-query wall-clock timing. By default
   (`--query-concurrency 1`) queries run one at a time on the main thread;
   with `--query-concurrency N > 1` they're dispatched across a pool of
   `N` threads instead (see below).
3. **Report** — write throughput, a min/avg/p50/p90/p99/p99.9/max latency
   table over all queries, a query throughput block (queries, concurrency,
   elapsed wall time, queries/sec), and 10 evenly-spaced sample queries
   with their result and latency, so you can eyeball what's actually being
   matched.

### APIs and datatypes used

- `redis.Redis` — a plain connection; the benchmark only calls `.ping()`,
  `.flushdb()`, and hands the connection to `RuleStore`.
- `fqdn_router.RuleStore` — `put_many(rules)` for pipelined bulk writes,
  `lookup(fqdn1, port, fqdn2, mode)` for a single timed lookup. Internally
  this issues one `HSET`/`EVALSHA` per rule/lookup via a Redis pipeline.
- `fqdn_router.Rule` — frozen dataclass: `fqdn1` (pattern), `port`,
  `fqdn2` (pattern), `rule_id`, `scope` (defaults to `WildcardScope.BOTH`,
  unused by the benchmark).
- `fqdn_pattern.MatchMode` — `SINGLE_LEVEL` or `MULTI_LEVEL`, selected by
  `--mode`; controls how deep a `*.`-wildcard rule reaches.
- `fqdn_router.Match` — returned by `lookup()` on a hit: `rule_id`,
  matched `fqdn1_pattern`/`fqdn2_pattern`, `port`, `specificity`. `None`
  on a miss.

### Rule depth / key layout (`--rule-depth-tiers`)

Each key ("standard zone") is one `(fqdn1 pattern, port)` pair, and can have
anywhere from a few to many hundreds of rules nested under it as distinct
`fqdn2` fields in the same Redis hash — modeling a zone that maps to a few,
a few dozen, or many hundreds of distinct endpoints, routes, or specific
rule exemptions. `--rule-depth-tiers` (default
`"50:1-3,30:4-20,10:21-50,10:51-2000"`) controls the mix: each `PCT:LO-HI`
tier says "this percentage of keys get a rule count sampled uniformly from
`[LO, HI]`". The default means 50% of keys hold 1-3 rules, 30% hold 4-20,
10% hold 21-50, and 10% hold 51-2000. Percentages are renormalized, so they
don't need to sum to exactly 100.

`--num-rules` is partitioned into keys by repeatedly picking a tier (by
weight) and drawing a size within it, until the sizes sum to `--num-rules`
(the last key is trimmed to fit exactly). This partition is deterministic
in `--num-rules` and `--rule-depth-tiers` alone (a fixed internal seed, not
`--seed` — see the `--seed` section below for why), so the same
`--num-rules`/`--rule-depth-tiers` always produces the same key layout.

`bucket = key_id % 1000`, `domain = dragonfly{key_id % 10000}.com` — keys
are spread across `NUM_DOMAINS = 10_000` distinct apex domains, so a run
with many thousands of keys lands on many distinct Dragonfly hash tags
(`{bench:dragonflyN.com}`) instead of piling every key onto one
`{bench:dragonfly.com}` tag/shard. `fqdn2` and `rule_id` are unique per
rule (not per key):

```python
Rule(
    fqdn1="*.host15.corp15.dragonfly15.com",   # shared by every rule under key 15
    port=443,                                  # cycles through 443, 8443, 80, per key
    fqdn2="*.dest727.example.com",              # unique per rule
    rule_id="rule:727",                         # unique per rule
)
```

### Catch-all tier replication (`--num-buckets`)

Every lookup unconditionally checks a global catch-all tier (`fqdn_pattern`'s
bare `"*"` candidate), and, under `--mode multi`, a per-TLD tier too (e.g.
`"*.com"`). Both are a single Dragonfly key regardless of dataset size, so
every single query, hit or miss, contacts the exact same key for those two
tiers -- unlike the apex tier above, which naturally spreads across one key
per domain. On a real multi-threaded Dragonfly server this concentrates a
disproportionate share of CPU onto the one or two threads that own those
fixed keys, capping throughput no matter how much client concurrency you
throw at it, since Dragonfly shards keys across its own threads and a given
key is only ever served by the thread that owns it.

`--num-buckets N` (default `16`) replicates those two tiers across `N`
physical keys instead of one. A write to a catch-all-tier rule fans out to
all `N` replicas, still in a single pipelined round trip. A lookup picks
exactly one replica, deterministically, by hashing the query's own fqdn, so
concurrent queries spread across up to `N` different Dragonfly threads
instead of converging on one. This doesn't change lookup latency or the
existing at-most-3-groups-per-lookup shape at all; it only changes which
physical key each of those groups lands on. `--num-buckets 1` disables
bucketing entirely and reproduces the pre-bucketing key names exactly.

Since `benchmark.py`'s synthetic rules never register a global or per-TLD
rule (every rule is apex-tier), bucketing here changes *read* distribution
only: the always-checked, always-empty catch-all-tier lookups spread across
`--num-buckets` keys instead of hammering one. Size it to your target
server's real thread count for the tightest fit (`fqdn_router.
recommended_num_buckets()` reads it from `INFO` for you); the default of 16
is a reasonable one-size guess for a typical modern core count, not a
substitute for checking the real number. Reusing rules written by an
earlier run (`--no-flush`) with a *different* `--num-buckets` than that
run used raises a clear error rather than silently misbehaving, since the
two runs would otherwise disagree on which physical key a given query's
catch-all check resolves to.

### Sample query executed against the dataset

A hit query targets the corresponding rule's key directly:

```
lookup("probe.host15.corp15.dragonfly15.com", 443, "probe.dest727.example.com", MatchMode.MULTI_LEVEL)
  -> Match(rule_id="rule:727", ...)
```

A miss query is a random unregistered name:

```
lookup("nomatch1281810600.unknown.invalid", 443, "nomatch1281810600.invalid", MatchMode.MULTI_LEVEL)
  -> None
```

### Sample run (2,000 rules, 200 queries, local Redis)

```sh
uv run benchmark.py --num-rules 2000 --num-queries 200 --seed 42
```

```
=== Key layout ===
standard zones (keys) : 43
rules per key         : avg 46.5, min 1, max 826
  tier 1 [1-3] (target 50.0%): 20 keys, 38 rules
  tier 2 [4-20] (target 30.0%): 15 keys, 126 rules
  tier 3 [21-50] (target 10.0%): 5 keys, 176 rules
  tier 4 [51-2000] (target 10.0%): 3 keys, 1,660 rules

writing 2,000 rules in batches of 5,000...
  write progress: 2,000/2,000 rules, 60.33 KB written, 0.0s elapsed

=== Write phase ===
rules written : 2,000
data written  : 60.33 KB
elapsed       : 0.01s
rate          : 176,972 rules/sec

running 200 queries (mode=multi, concurrency=1)...

=== Query latency (ms) ===
     min      avg      p50      p90      p99    p99.9      max
   0.353    0.423    0.414    0.477    0.640    0.840    0.840

=== Query throughput ===
queries       : 200
concurrency   : 1
elapsed       : 0.08s
rate          : 2,360 queries/sec

=== Sample results (10 evenly-spaced queries) ===
   idx  fqdn1                                          port fqdn2                          result               latency_ms
     0  probe.host15.corp15.dragonfly15.com             443 probe.dest727.example.com      rule:727                  0.840
    22  probe.host20.corp20.dragonfly20.com              80 probe.dest1206.example.com     rule:1206                 0.438
   133  nomatch1281810600.unknown.invalid               443 nomatch1281810600.invalid      MISS                      0.463
   199  probe.host20.corp20.dragonfly20.com              80 probe.dest877.example.com      rule:877                  0.353
```

Note that several sampled queries above (idx 22 and 199) land on the same
key (`host20.corp20.dragonfly20.com`) but resolve to different rules —
exactly the "many rules nested under one key" shape `--rule-depth-tiers`
produces.

### `--seed` (reproducible query selection)

`--seed` drives the `random.Random` used to pick which rule each hit query
targets, generate miss queries, and shuffle the combined hit/miss list. The
same seed against the same `--num-rules`/`--num-queries`/`--miss-rate`
always produces the exact same sequence of `(fqdn1, port, fqdn2)` queries
in the exact same order, so two runs are directly comparable (only
latency, which depends on the live Redis/Dragonfly instance, will differ
between runs). A different seed produces a different query sequence.

The key layout (see `--rule-depth-tiers` above) is deliberately
*independent* of `--seed` — it depends only on `--num-rules` and
`--rule-depth-tiers` — so a `--no-flush` rerun with a different `--seed`
still resamples queries against the exact same previously-written keys
instead of silently comparing against a different layout.

Two runs with `--seed 7`, same query at index 0 both times:

```
$ uv run benchmark.py --num-rules 2000 --num-queries 200 --seed 7   # run 1
     0  probe.host703.corp703.dragonfly.com            8443 probe.dest703.example.com      rule:703                  0.693

$ uv run benchmark.py --num-rules 2000 --num-queries 200 --seed 7   # run 2
     0  probe.host703.corp703.dragonfly.com            8443 probe.dest703.example.com      rule:703                  0.689
```

The same run with `--seed 8` instead picks a different query at index 0:

```
$ uv run benchmark.py --num-rules 2000 --num-queries 200 --seed 8
     0  nomatch72766124.unknown.invalid                 443 nomatch72766124.invalid        MISS                      0.540
```

Default is `--seed 42` (used by the sample run above); pass a different
value to get a different fixed query set, or vary it across CI runs to
cover more of the query space over time while keeping any single run
reproducible.

### `--query-concurrency` (multi-threaded query phase)

`--query-concurrency N` (default `1`) controls how many threads issue
lookups against `RuleStore` at once during the query phase. `N == 1` is
the original behavior: one query at a time on the main thread, so each
latency sample is a clean, uncontended round trip. `N > 1` dispatches the
same query list across a pool of `N` threads (`redis.Redis` pools
connections internally, so concurrent `store.lookup()` calls from
different threads are safe); the reported `queries/sec` is
`num_queries / wall_clock_elapsed` for the whole phase, so it captures the
actual overlap. Per-query latency naturally rises with concurrency (queries
now queue for a shared connection pool and contend for server-side CPU),
so use the latency table to judge single-query cost and the throughput
block to judge aggregate capacity.

Same 3,000-query run at `--query-concurrency 1` vs `--query-concurrency 32`
against the same dataset:

```
$ uv run benchmark.py --num-rules 5000 --num-queries 3000 --seed 42 --query-concurrency 1
=== Query throughput ===
queries       : 3,000
concurrency   : 1
elapsed       : 1.31s
rate          : 2,295 queries/sec

$ uv run benchmark.py --num-rules 5000 --num-queries 3000 --seed 42 --query-concurrency 32 --no-flush
=== Query throughput ===
queries       : 3,000
concurrency   : 32
elapsed       : 0.41s
rate          : 7,282 queries/sec
```

`--no-flush` above reuses the rules already written by the first run
instead of re-running the write phase, so both runs query the identical
dataset. `--query-concurrency` is capped at `256`.

### `--query-source disk` (stage queries through a file)

By default (`--query-source memory`) the shuffled query list is built once
and handed straight to the query phase. `--query-source disk` instead
writes that same list to `--query-file` (a plain tab-separated file,
default `benchmark_queries.tsv`) in batches of `--query-file-batch-size`
(default `10000`), pauses for `--query-pause-seconds` (default `5.0`), and
then reads the file back in batches of the same size to repopulate the
in-memory list before the query phase starts. The query sequence itself is
unaffected: the file round-trip preserves order, so the same `--seed`
produces the same queries whichever `--query-source` you pick.

The pause exists so the write side of the round-trip — and whatever
system-level settling you want (e.g. page cache, disk flush) — finishes
cleanly before the timed lookups begin; it's excluded from every reported
metric.

```sh
uv run benchmark.py --num-rules 2000 --num-queries 200 --seed 42 \
  --query-source disk --query-file /tmp/queries.tsv \
  --query-file-batch-size 5000 --query-pause-seconds 2
```

```
writing 200 pre-calculated queries to /tmp/queries.tsv in batches of 5,000...
pausing 2.0s before the query phase...
reading queries back from /tmp/queries.tsv in batches of 5,000...

running 200 queries (mode=multi, concurrency=1)...
```

### `--query-source reuse` (skip generation, reread a file from a prior run)

`--query-source reuse` skips query generation and the write step entirely:
it reads `--query-file` directly, in batches of `--query-file-batch-size`,
straight into the in-memory query list. Use it to rerun the exact same
query sequence — e.g. against a different Redis/Dragonfly target, or after
a code change to `RuleStore` — without paying the cost of recalculating
them, and typically alongside `--no-flush` so the rules underneath the
queries are the same ones already written by the `disk` run that produced
the file. There's no pause in this mode since nothing is written; the
benchmark errors up front if `--query-file` doesn't exist.

```sh
# first run: precompute both the rules and the query file
uv run benchmark.py --num-rules 2000 --num-queries 200 --seed 42 \
  --query-source disk --query-file /tmp/queries.tsv --query-pause-seconds 0

# later run: reuse the same query file against the same (unflushed) rules
uv run benchmark.py --num-rules 2000 --num-queries 200 --seed 42 --no-flush \
  --query-source reuse --query-file /tmp/queries.tsv
```

Both runs produce the identical sample-query table (same `fqdn1`/`port`/
`fqdn2`/result at every sampled index), confirming the reused file
reproduces the original query sequence exactly.
