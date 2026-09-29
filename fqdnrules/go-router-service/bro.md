# The go-router-service client, explained to a bro

Here's the deal with what `routerd` is actually doing against Dragonfly/Redis —
same job as `go-benchmark`'s client (see its own `bro.md`), same stored data, same
answers, totally different mechanics inside.

**Writes** are identical to the Lua version, byte for byte — same `HSET` calls,
same hash-tagged key layout. `routerd` doesn't even expose a write path; whatever
wrote the data (`go-benchmark`'s write phase) is still the only writer.

**Lookups** are where this one earns its name: no server-side script at all.
Instead of shipping one `EVALSHA` per hash-tag group and letting Lua do the
candidate-checking inside Dragonfly, `NoScriptRuleStore.Lookup` builds the exact
same candidate lists in Go, then pipelines one plain `HMGET` per fqdn1 candidate
key — all of them, not grouped by hash tag — into a single round trip, and does
every bit of scope-filtering and specificity ranking in Go once the values come
back. Dragonfly's job shrinks to "answer some `HMGET`s"; nothing it returns is
ever partially interpreted or executed.

Why no grouping by hash tag this time? Grouping into ≤3 buckets was only ever
needed because a Lua script's `KEYS` all have to land on one Redis Cluster slot.
`HMGET` is a single-key command — each one is free to hit whatever slot its own
key lands on — so there's nothing to group.

## Concrete sample

Same stored data as `go-benchmark/bro.md`'s example, so the two architectures can
be compared directly on the identical query:

```
key:   r:{bench:dragonfly42.com}:*.host7.corp3.dragonfly42.com:443

HSET r:{bench:dragonfly42.com}:*.host7.corp3.dragonfly42.com:443 \
     "*.dest42.example.com" "Brule:42"

HSET r:{bench:dragonfly42.com}:*.host7.corp3.dragonfly42.com:443 \
     "api.dest43.example.com" "Srule:43"
```

### A single query

```
store.Lookup("probe.host7.corp3.dragonfly42.com", 443, "probe.dest42.example.com", MultiLevel)
```

`pattern.go` is copied unchanged from `go-benchmark`, so the candidate lists are
identical: 6 fqdn1 candidates (exact, `*.host7.corp3.dragonfly42.com`,
`*.corp3.dragonfly42.com`, `*.dragonfly42.com`, `*.com`, `*`) and 5 fqdn2
candidates (`probe.dest42.example.com`, `*.dest42.example.com`, `*.example.com`,
`*.com`, `*`), both already sorted most-specific-first. But instead of splitting
the fqdn1 side into hash-tag groups for separate `EVALSHA`s, every one of those 6
keys gets its own `HMGET` against all 5 fields, all pipelined together:

```
HMGET r:{bench:dragonfly42.com}:probe.host7.corp3.dragonfly42.com:443 \
      probe.dest42.example.com *.dest42.example.com *.example.com *.com *

HMGET r:{bench:dragonfly42.com}:*.host7.corp3.dragonfly42.com:443 \
      probe.dest42.example.com *.dest42.example.com *.example.com *.com *

HMGET r:{bench:dragonfly42.com}:*.corp3.dragonfly42.com:443 \
      probe.dest42.example.com *.dest42.example.com *.example.com *.com *

HMGET r:{bench:dragonfly42.com}:*.dragonfly42.com:443 \
      probe.dest42.example.com *.dest42.example.com *.example.com *.com *

HMGET <the *.com tld-tier key> \
      probe.dest42.example.com *.dest42.example.com *.example.com *.com *

HMGET <the *   global-tier key> \
      probe.dest42.example.com *.dest42.example.com *.example.com *.com *
```

No bit-strings, no `ARGV`, no `EVALSHA` — just six ordinary commands in one
pipeline (`store.go:255-264`).

#### Why there are no binary flags here

`go-benchmark`'s Lua script needed `kmulti`/`kexact`/`fmulti` as comma-joined
`0`/`1` strings because the candidate list was built in one runtime (Go) and
checked in another (the Lua VM inside Dragonfly) — crossing that boundary means
crossing a wire, and a wire only carries flat strings. Here, both halves happen
in the same Go process: `candidates()` already hands back `Candidate` structs
with a real `Rank int` and `NeedsMulti bool` field, and `Lookup`'s own loop reads
them directly, no encoding or decoding involved:

```go
exact := a.Rank == 0 && b.Rank == 0
deep := a.NeedsMulti || b.NeedsMulti
```

Same two booleans, same meaning as the Lua version's `exact`/`deep` — "no wildcard
involved on either side" and "at least one side only matched via a wildcard
reaching more than one label down" — just never serialized, because there was
never a process boundary to cross in the first place.

### What comes back

Values come back in the same order the `HMGET`s were issued. The exact-match key
(`probe.host7.corp3.dragonfly42.com:443`) doesn't exist. The second key — our key
with 2 rules — hits on field `*.dest42.example.com` (rank 1) → value
`"Brule:42"`. `exact` is false (rank 1 candidate, not rank 0); `deep` is false
(neither side needed a multi-level wildcard here); scope is `'B'`, which is never
blocked. So the loop returns immediately, before it even looks at the remaining
`*.com`/`*` keys' values:

```go
Match{
    RuleID:       "rule:42",
    Fqdn1Pattern: "*.host7.corp3.dragonfly42.com",
    Port:         443,
    Fqdn2Pattern: "*.dest42.example.com",
    Rank1:        1,
    Rank2:        1,
}
```

Identical to what `go-benchmark`'s Lua path returns for the same query — exactly
what `golden_test.go` exists to keep proving.

## How "most specific wins" works here

Same `Rank` field, same meaning, same fixed `255` for the bare `*` — this is
shared code (`pattern.go`), not reimplemented. The difference is what happens
around it. `go-benchmark`'s Lua script can only guarantee the most-specific hit
*within one hash-tag group*, so the Go client still needs `moreSpecific()`
afterwards to pick a winner across up to three separate `EVALSHA` results.

Here there's only one flat loop over *all* the candidates — `aCandidates` outer,
`bCandidates` inner, both already in ascending-rank order — and it returns on the
first unblocked hit it finds (`store.go:220-234`). Walking two lists that are both
sorted most-specific-first, outer-first, and stopping at the first hit is already
exactly the `(Rank1, Rank2)` minimum: fixing the most specific fqdn1 candidate
first, and only searching fqdn2 candidates within it before moving to a less
specific fqdn1 candidate. There's no second merge step, no `moreSpecific()`
equivalent, because there's no per-group split left to merge back together.

The trade this makes: since every `HMGET` is fired before any result comes back,
this always pays for `len(aCandidates)` round-trip-free commands (up to ~21 in
the worst case at `maxLabels`), even when — like in this example — the answer
was sitting in the *second* key checked. The Lua version, by contrast, can skip
querying a group's remaining keys once it finds a hit, but only within that one
group; across groups it's exactly as blind as this one is.
