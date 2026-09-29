# Proposal: shard global/TLD rules, replicate each shard

**Status: design proposal, not implemented anywhere in this repo.** Nothing
described here exists in `go-benchmark/` or `go-router-service/` today — this
document is a starting point for a decision, not a description of shipped
behavior. Companion reading: `bucketing_and_routerd_explained.md` §1 describes
the *current* bucketing scheme this proposal changes.

---

## 1. The problem

Today, `-num-buckets N` turns each of the two universally-checked tiers
(`__global__`, `__tld__:<x>`) into `N` **full replica** keys, not `N`
partitions. `keysForPattern` returns all `N` keys for a bucketed tag
(`go-benchmark/router.go:198-208`, identically at
`go-router-service/store.go:129-139`), and `Put`/`PutMany` write every rule
into every one of them (`router.go:210-231,235-258`; `store.go:141-189`). So
every bucket key holds an **identical copy of the entire catch-all rule set**
for that tier.

That was the right fix for the problem it targeted — spreading the
*always-hit* tiers across Dragonfly's threads instead of pinning 100% of that
traffic to one or two keys (`bucketing_and_routerd_explained.md:20-58`). It
does nothing for a different problem: **the field count of any one bucket key
never shrinks**, no matter how large `N` gets, because every bucket still
holds every global/TLD rule. If the global tier accumulates millions (or
billions) of `(fqdn2_pattern -> rule)` entries, every one of the `N` bucket
keys carries all of them.

The apex tier is unaffected by anything in this document — it already
partitions naturally, one key per registrable domain
(`pattern.go:118-121`'s own comment: "the apex tier already spreads across one
key per registrable domain and must never be bucketed").

## 2. The proposal: two independent indices, not one

Keep the existing **replica bucket** index exactly as it is — it's a good
mechanism for what it does (thread/CPU spread) and nothing here needs to
change about it: a query still computes `bucket_id = hash(query_fqdn1) % N`
(`pattern.go:149-155`), landing deterministically on one of `N` slot-targeted
physical copies (`slots.go:1-19,140-145,166-207`).

Add a second, independent **content shard** index, computed once per rule at
write time from its own `fqdn2` pattern:

```go
// illustrative, not existing code
shard_id := int(crc32.ChecksumIEEE([]byte(rule.Fqdn2))) % M
```

`M` (shard count) is a completely separate knob from `N` (bucket count) —
choose it purely to cap how many fields any one physical key can hold,
independent of how many Dragonfly threads exist. Then **replicate each shard
across all `N` buckets**, exactly the way the whole tier is replicated today
— just one level further in:

- Today's physical key set for a bucketed tag: `N` keys, each holding *every*
  rule.
- Proposed physical key set: `N x M` keys, each holding `~1/M` of the rules,
  with all `M` shards present under every one of the `N` buckets.

### Why two indices instead of one

An earlier version of this proposal considered hashing `fqdn2` directly into
the *existing* bucket index — i.e., partitioning without replicating,
`bucket_id = hash(rule.Fqdn2) % N`, one copy per rule, no duplication. That
version runs into a real conflict: arch 2's Lua path groups fqdn1 candidates
by hash tag specifically so one `EVALSHA` call's `KEYS` all land on the same
Redis Cluster slot (`router.go:314-333`;
`bucketing_and_routerd_explained.md:93-105`). If a single tier's fqdn2
candidates can legitimately hash into *different* buckets — and different
buckets are deliberately placed on different slots, that's the whole point of
`slots.go`'s token table — then the keys one lookup needs to touch may no
longer share a slot, and `EVALSHA` cannot be handed `KEYS` spanning slots.

Keeping the two indices separate avoids this entirely: `shard_id` never
enters the `{}` hash-tag braces, so it never affects slot placement — only
`bucket_id` does, unchanged from today. Every shard-key for the *same*
`bucket_id` therefore shares one slot, no matter how many shards there are.
That's what makes bundling multiple fqdn2 candidates back into a single
`EVALSHA` call possible again (§4b).

## 3. Key format

```
r:<ns>:{<bucket_token>}:<tag>:<bucket_id>:<shard_id>:<fqdn1_pattern>:<port>
```

Same shape as today's bucketed key
(`r:<ns>:{<token>}:<tag>:<bucket_index>:<fqdn1_pattern>:<port>`,
`router.go:184-185`; `store.go:113-119`), with `shard_id` inserted as one more
plain key-body segment — same treatment `bucket_id` itself already gets
("namespace, tag, bucketIndex, pattern and port all stay outside the braces
as plain key-body text, so a SCAN `r:<ns>:*` prefix scan still finds them",
`store.go:113-119`'s doc comment). `<bucket_token>` is still the slot-targeted
string from `ComputeBucketTable(N)` — completely unchanged.

## 4. Impact on writes

`Put`/`PutMany`/`Delete` keep the *same fan-out count* they have today — `N`
writes per rule, one per bucket replica — but each write now targets the one
shard-key that rule belongs to, instead of a key holding the whole tier:

```go
// illustrative
func (s *RuleStore) shardFor(fqdn2 string) int {
    return int(crc32.ChecksumIEEE([]byte(fqdn2))) % s.numShards
}

func (s *RuleStore) keysForBucketedRule(tag, fqdn1Pattern, fqdn2 string, port int) []string {
    shard := s.shardFor(fqdn2)
    keys := make([]string, s.numBuckets)
    for bucket := range keys {
        keys[bucket] = s.key(tag, bucket, shard, fqdn1Pattern, port)
    }
    return keys
}
```

- **`Put`**: unchanged shape — one `HSet` per bucket, `N` total — just each
  one now lands on a key holding `~1/M` of the tier instead of all of it.
- **`PutMany`**: same pipeline structure as today
  (`router.go:235-258`/`store.go:166-189`); each rule still contributes `N`
  `HSet`s, computed once per rule (`shard_id` doesn't vary across the `N`
  writes for the same rule — only `bucket_id` does).
- **`Delete`**: recompute the same `shard_id` from the rule's `fqdn2`, then
  `HDel` from all `N` bucket replicas of that one shard — same "true if the
  field existed in *any* replica" logic already in place
  (`router.go:275-281`), just scoped to `N` keys instead of `N` keys holding
  everything.
- **Bucketing metadata**: extend `bucketingMetaKey()` to also record `M`
  (shard count) alongside `N` (`num_buckets`) — both writer and reader must
  agree on *both* numbers now, since a reader computing a different `M` would
  look in the wrong shard-key entirely, the same failure mode the existing
  `num_buckets`-mismatch guard already exists to catch
  (`router.go:112-146`; `store.go:80-103`).

Net effect on writes: **no change** in command count versus today — still
`N` writes per rule. The whole benefit shows up in what each of those `N x M`
keys holds, not in how many writes happen.

## 5. Impact on reads

### 5a. Arch 3 (`routerd`, no Lua)

Pick `bucket_id` exactly as today (any bucket is still a complete copy of the
*whole* tier, across all its shards, so the existing "hash the query fqdn1"
logic needs no change at all). For each fqdn2 candidate, additionally compute
its `shard_id` and target that one field directly:

```go
// illustrative
bucket := int(crc32.ChecksumIEEE([]byte(normalized1))) % s.numBuckets
for i, a := range aCandidates {
    if isBucketedTag(a.Tag) && s.numBuckets > 1 {
        for j, b := range bCandidates {
            shard := s.shardFor(b.Pattern)
            key := s.key(a.Tag, bucket, shard, a.Pattern, port)
            cmds[i][j] = pipe.HGet(s.ctx, key, b.Pattern)
        }
    } else {
        key := s.key(a.Tag, 0, 0, a.Pattern, port)
        cmds[i] = pipe.HMGet(s.ctx, key, fields...)
    }
}
```

Still one pipeline, one round trip — `HGet`/`HMGet` never had a co-location
requirement (`store.go:198-206`). Command count for the two bucketed tiers
grows from `2` to roughly `2 x len(bCandidates)`, same growth this proposal
always implied once field-level targeting is needed at all — the shard/bucket
split doesn't add to this cost, it only avoids the arch-2 problem below.

### 5b. Arch 2 (Lua/`EVALSHA`) — now works

Pick `bucket_id` the same way. For a bucketed tier, `KEYS` becomes the set of
shard-keys actually touched by this query's fqdn2 candidates — at most
`min(len(bCandidates), M)` of them — all sharing the one chosen `bucket_id`,
hence all on one slot:

```go
// illustrative
bucket := int(crc32.ChecksumIEEE([]byte(normalized1))) % numBuckets
shardsNeeded := map[int]bool{}
for _, b := range bCandidates {
    shardsNeeded[shardFor(b.Pattern)] = true
}
keys := make([]string, 0, len(shardsNeeded))
for shard := range shardsNeeded {
    keys = append(keys, key(tag, bucket, shard, fqdn1Pattern, port))
}
// one EVALSHA, KEYS = keys, all on the same slot
```

The Lua script itself barely changes: instead of one `HMGET` per KEYS entry
checking every field (today's `luaLookup`, `router.go:59-77`), it needs to
know *which* fields to look for in *which* KEYS entry, since different shards
hold different fqdn2 candidates. That's an extra small ARGV mapping (which
candidate belongs to which KEYS index) — a bookkeeping change, not a
structural one; the scope-filtering logic (`exact`/`deep`/`blocked`) is
untouched.

The "≤3 total `EVALSHA` calls per lookup" invariant
(`bucketing_and_routerd_explained.md:145-147`) is no longer a hard `3` — it's
now bounded by the number of *distinct shards* a query's candidates touch,
per bucketed tier. In the common case (`M` chosen sensibly relative to
`len(bCandidates)`) that's small and often still `1`; the worst case is
`len(bCandidates)`, same bound as arch 3.

## 6. Consequences, side by side

| | Today (replicate whole tier) | Rejected: partition only, no replication | Proposed: shard + replicate |
|---|---|---|---|
| Field count per physical key | full rule set, every bucket | `~total / N` | `~total / M` |
| Writes per rule (bucketed tier) | `N` | 1 | `N` (unchanged from today) |
| Physical keys per bucketed tier | `N` | `N` | `N x M` |
| Reads per bucketed tier, arch 3 | 1 `HMGET` | up to `len(bCandidates)` `HGet`s | up to `len(bCandidates)` `HGet`s |
| Reads per bucketed tier, arch 2 | 1 `EVALSHA` | broken (slot conflict) or up to `len(bCandidates)` calls | up to `min(len(bCandidates), M)` calls, sharing one slot |
| Resizing `N` (thread spread) later | free | requires full rehash | free — unchanged from today |
| Resizing `M` (size cap) later | n/a | requires full rehash | requires full rehash of shard placement, but `N` is untouched |
| Memory for the catch-all tiers | `O(N x rules)` | `O(rules)` | `O(N x rules / M x M)` = `O(N x rules)` total, but `O(rules/M)` per key |

The shard+replicate design keeps today's total storage footprint (still `N`
copies of the tier overall) and today's write cost, and buys a hard cap on
any single key's field count via `M`, chosen independently of thread count —
at the cost of read commands per bucketed tier growing from `O(1)` to
`O(len(bCandidates))` in both architectures, and a migration whenever `M`
changes (not when `N` changes).

## 7. Open questions this proposal does not resolve

- **Choosing `M`.** Needs a concrete target (e.g. "no key should exceed X
  fields") and a way to estimate rule-count growth per tier to pick it
  sensibly, plus a plan for what happens when that estimate turns out wrong.
- **Port stays outside both hashes.** A rule's shard is chosen from `fqdn2`
  alone; `port` is still a separate key dimension. If many ports each carry
  only a handful of global/TLD rules, sharding by `fqdn2` alone still
  produces `M` mostly-empty shard-keys per port. Worth deciding whether to
  shard on `(port, fqdn2)` jointly, or only shard a given port's catch-all
  tier once its rule count crosses some threshold.
- **Resharding when `M` changes.** Because a rule's shard is fixed by content,
  growing or shrinking `M` requires rewriting every affected rule into its
  new shard-key — this needs its own migration story (a background rewrite
  job, a dual-write window, or an offline rebuild) before `M` can be treated
  as safely as `N` already is today.
- **The Lua script's ARGV bookkeeping** (§5b's "which candidate belongs to
  which KEYS index") needs to be worked out concretely and tested — sketched
  here, not designed in full.
- **No numbers exist yet.** Like the rest of this project's architecture
  comparisons, whether this is worth the read-command growth, versus simply
  raising `N` alone or accepting large keys, has not been measured against
  any real field-count distribution.
