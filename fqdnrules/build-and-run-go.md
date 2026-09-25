# Build and run the Go code

Two independent Go modules, `go-router-service/` and `go-benchmark/`. `go build` is identical on macOS and Linux; only installing Go itself differs.

## Install Go (skip if already installed)

macOS:
```
brew install go
```

Linux (Debian/Ubuntu):
```
sudo apt install golang-go
```

## Build

```
cd fqdnrules/go-router-service && go build -o routerd ./cmd/routerd
cd fqdnrules/go-benchmark && go build -o fqdnbench .
```

## Run, 32 buckets, against a Redis/Dragonfly URI

`-num-buckets` must match between whatever writes the data and whatever reads it — both binaries below use 32 as the example.

**routerd** (gRPC front end over `NoScriptRuleStore`):
```
./go-router-service/routerd -uri redis://localhost:6379/0 -namespace bench -num-buckets 32 -listen :9090
```

**fqdnbench** (writes rules, then times lookups):
```
./go-benchmark/fqdnbench -uri redis://localhost:6379/0 -num-buckets 32
```

Use `rediss://` in place of `redis://` for a TLS-enabled endpoint. Swap in your real host/port/db and credentials, e.g. `redis://:mypassword@my-dragonfly-host:6379/0`.
