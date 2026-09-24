# Go FQDN-pair router benchmark

This is a Go port of the Python FQDN-pair router and its benchmark CLI one directory up: `fqdn_pattern.py`, `fqdn_router.py`, and `benchmark.py`. It writes N synthetic `(fqdn1, port, fqdn2)` rules into Redis or Dragonfly, runs M timed lookups against them, and reports write throughput and lookup latency percentiles. The matching semantics, the Redis key names, and the Lua lookup script are the same as the Python version, byte for byte, confirmed by `golden_test.go`: for the *same* rules and queries handed to each `RuleStore` directly, both implementations write identical keys and agree on every match. The port exists to take the GIL out of the query phase. See "Why this port exists" below.

**This does not mean the two benchmark CLIs' own generated datasets are cross-compatible.** `benchmark.py`'s and this port's write phase both decide which rule count nests under which key via a PRNG-driven layout (`build_key_sizes`), and Go's `math/rand` and Python's `random.Random` are different algorithms: the same `-seed` and `-rule-depth-tiers` produce a different layout in each language, even though the key *format* is identical. Writing a dataset with one implementation's benchmark CLI and querying it with the other's, e.g. `./benchmark ...` then `benchmark.py ... --no-flush`, will not line up and reads as near-total misses. That's a mismatched-layout artifact, not a correctness bug; confirmed directly running that exact sequence. Benchmark each implementation against data it wrote itself, or use `RuleStore`/`fqdn_router.RuleStore` directly (as `golden_test.go` and `crosscheck_python.py` do) if you need genuine cross-implementation data sharing.

## Before you start

You need a reachable Redis or Dragonfly instance. By default the benchmark flushes the database you point it at before it writes. Pass `-no-flush` to query data that is already there.

You also need Go 1.21 or later. The next two sections install it.

## Install Go on macOS

```
brew install go
go version
```

## Install Go on Ubuntu 20.04

Do not run `apt install golang-go`. Ubuntu 20.04 ships Go 1.13, which cannot compile `github.com/redis/go-redis/v9`. That package needs generics, which arrived in Go 1.18.

Download the official tarball, extract it to `/usr/local`, and add `/usr/local/go/bin` to your `PATH`:

```
wget https://go.dev/dl/go1.21.13.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.21.13.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.profile
source ~/.profile
go version
```

On an arm64 host, use `go1.21.13.linux-arm64.tar.gz` instead.

## Build

The same command works on macOS and Linux:

```
cd go-benchmark
go build -o benchmark .
```

To cross-compile from macOS for a Linux server, set `GOOS` and `GOARCH`:

```
GOOS=linux GOARCH=amd64 go build -o benchmark-linux .
```

Building natively on each target is simpler, and that is what most people should do.

## Run

A small local smoke test, against database 13 so it cannot flush anything you care about:

```
./benchmark -num-rules 2000 -num-queries 500 -db 13
```

A realistic run against a remote host, with 32 goroutines issuing queries:

```
./benchmark -uri redis://:password@dragonfly.example.com:6379/0 \
  -num-rules 1000000 -num-queries 200000 -query-concurrency 32 -num-buckets 8
```

Set `-num-buckets` to the target server's own thread count, which `INFO server` reports as `thread_count`. That value controls how many physical keys the global and per-TLD catch-all tiers are replicated across. Every lookup checks those two tiers no matter which domain it asks about, so without replication they concentrate all that traffic on one key and one shard.

## Flags

| Flag | Default | What it does |
| --- | --- | --- |
| `-num-rules` | `100000` | Rules to write. Must be within [1000, 500000000]. |
| `-num-queries` | `10000` | Lookups to time. Must be within [100, 50000000]. |
| `-host` | `localhost` | Server host. |
| `-port` | `6379` | Server port. |
| `-db` | `0` | Database index. |
| `-uri` | empty | A `redis://` or `rediss://` URI, replacing `-host`, `-port`, and `-db`. `rediss://` turns on TLS. |
| `-mode` | `multi` | `single` substitutes exactly one label, per RFC 6125. `multi` matches any descendant depth. |
| `-miss-rate` | `0.1` | Fraction of queries built to miss. Must be within [0.0, 1.0]. |
| `-write-batch-size` | `5000` | Rules per pipelined write batch. |
| `-rule-depth-tiers` | `50:1-3,30:4-20,10:21-50,10:51-2000` | Weighted `PCT:LO-HI` bands for how many rules nest under one key. Percentages are renormalized, so they need not sum to 100. |
| `-global-rule-rate` | `0.05` | Fraction of rules registered as global-tier `*` catch-alls. Must be within [0.0, 1.0]. |
| `-tld-rule-rate` | `0.10` | Fraction of rules registered as per-TLD `*.<tld>` catch-alls. Must be within [0.0, 1.0], and the two rates together must not exceed 1.0. |
| `-num-buckets` | `16` | Replicas of the global and per-TLD catch-all tiers. `1` disables replication. |
| `-seed` | `42` | Seed for query selection and shuffling. |
| `-no-flush` | off | Skip the flush and the write phase, and query data already in the database. |
| `-query-concurrency` | `1` | Goroutines issuing queries at once. Must be within [1, 256]. |
| `-target` | `direct` | `direct` queries Dragonfly directly through `RuleStore` (Lua, this implementation). `grpc=host:port[,host:port,...]` instead round-robins queries across one or more `go-router-service` `routerd` instances (no Lua, see `../go-router-service/README.md`) — a third, comparable architecture. The write phase always goes direct regardless of `-target`, since `routerd` has no write RPC by design. |

## Reading the output

The run prints four blocks: the key layout, the write phase, query latency percentiles, and ten evenly spaced sample lookups.

The throughput block reports an `errors` count. A run that reports errors also exits with status 1, because a run whose lookups all failed would otherwise post an excellent queries per second figure and read as a success.

`-global-rule-rate` and `-tld-rule-rate` decide how many rules land in each hash-tag tier, and the key layout block reports the split. At the defaults, 5% of rules register under the global pattern `*` and 10% register under a per-TLD pattern `*.<tld>`. The other 85% register under an apex pattern like `*.host12.corp12.dragonfly12.com`, whose hash tag is the apex `dragonfly12.com`. A default run therefore writes real global-tier and per-TLD keys, so `-num-buckets` replication shows up in both the write phase and every lookup. Set both rates to `0` to recover the old apex-only workload, which writes zero global-tier and zero per-TLD keys and matches what `benchmark.py` does. `router_test.go` covers the fan-out directly.

## Verify the port against the Python implementation

`golden_test.go` replays a fixed rule set and query set through the Go `RuleStore` and compares every result against what the Python `RuleStore` produced for the same input. It checks three things: that the two implementations write identical physical keys, that the Go store reads the keys Python wrote, and that both give the same match for all 28 queries.

```
python3 crosscheck_python.py /tmp/crosscheck.json
FQDN_GOLDEN=/tmp/crosscheck.json go test -run TestGoldenCrossCheck -v
```

Set `FQDN_NUM_BUCKETS` to run it at a different bucket count. Both halves use database 12.

The rest of the suite needs a server on `localhost:6379` and uses database 14:

```
go test ./... -v
```

## What this port leaves out

The Python benchmark's `--query-source disk` and `--query-source reuse` modes stage queries through a file. This port builds queries in memory only and has no `-query-source`, `-query-file`, `-query-file-batch-size`, or `-query-pause-seconds`. File staging is unrelated to what the port is for, so it was dropped rather than translated.

`-seed` gives you a deterministic Go-side dataset and query sequence. It does not reproduce the Python benchmark's dataset at the same seed, because the two languages use different pseudorandom number generators. The key layout is independent of `-seed` in both implementations, so a `-no-flush` rerun with a different `-seed` still samples queries against the same previously written keys.

## Why this port exists

The Python client is bound by the GIL. We confirmed that against this deployment by splitting one process's 30 query threads into two processes of 15 threads each, which doubled throughput against the same server. Nothing changed on the server, so the ceiling was in the client.

Go's goroutines have no such ceiling, so one Go process should reach the throughput that took two Python processes at the same total concurrency. To confirm it, run `benchmark.py` and `./benchmark` against the same server with the same `-num-rules`, `-num-queries`, and concurrency, and compare the queries per second each one reports.
