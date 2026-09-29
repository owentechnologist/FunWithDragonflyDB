# The go-benchmark client, explained to a bro

Here's the deal with what this Go client is actually doing against Dragonfly/Redis:

**Writes** are dead simple — it's just `HSET` calls (a field into a hash), batched up
into big pipelines (5000 at a time) so it's not round-tripping the network per-rule.
Each rule lands in a hash keyed by its domain pattern + port. Nothing fancy, just
"shove data in."

**Lookups** are the interesting part. Instead of the client doing a bunch of round
trips to check "does this candidate match, does that one," it sends over a **Lua
script** (via `EVALSHA`, loaded once and reused) that does all the candidate-checking
*inside* Redis/Dragonfly in one shot. So a single lookup query = 1 to 3 of these
script calls, all bundled into one pipeline = **one network round trip total**, no
matter how many candidate keys it's actually checking.

Why 1-3 and not more? The keys are deliberately "hash-tagged" into three buckets —
your domain's own bucket, its TLD's bucket, and a global catch-all bucket. That
tagging trick means Redis Cluster/Dragonfly is guaranteed to put each of those groups
on one shard, so the query never has to fan out across a ton of shards. It's about
*containing the blast radius* of one lookup to at most 3 slots.

**What comes back** is intentionally skinny: the script doesn't return the matched
data, it returns a little 3-element pointer — "which key won, which field won,
here's the rule ID." That's it. The client then looks up locally what pattern that
corresponds to (since it already knows what it sent). On a miss, you just get `nil`
back. And even when multiple candidates would match, the script only hands back the
*single most specific* one — there's no "give me everything that matches," it's
always narrowed down to one winner before it ever leaves the server.

So net-net: cheap batched writes, one-round-trip smart lookups that do their
filtering server-side in Lua, and results that are just "match ID or nothing" —
never a fat payload.

## Concrete sample

### A key with 2 rules

Both rules share the same `fqdn1` pattern (`*.host7.corp3.dragonfly42.com`) and port
(`443`), so they live as two fields in the same hash — that hash's name is the
"key" (bucket = `dragonfly42.com`, the pattern's last two labels):

```
key:   r:{bench:dragonfly42.com}:*.host7.corp3.dragonfly42.com:443

HSET r:{bench:dragonfly42.com}:*.host7.corp3.dragonfly42.com:443 \
     "*.dest42.example.com" "Brule:42"

HSET r:{bench:dragonfly42.com}:*.host7.corp3.dragonfly42.com:443 \
     "api.dest43.example.com" "Srule:43"
```

Each stored value is `<scope_char><rule_id>` mashed together — `B` = matches under
either single- or multi-level lookups, `S` = single-level only. So `"Brule:42"` means
"rule:42, scope Both."

### A single query

```
store.Lookup("probe.host7.corp3.dragonfly42.com", 443, "probe.dest42.example.com", MultiLevel)
```

The client works out every pattern that could match `probe.host7.corp3.dragonfly42.com`
(exact, `*.host7.corp3.dragonfly42.com`, `*.corp3.dragonfly42.com`,
`*.dragonfly42.com`, `*.com`, `*`) — all 4 of the `dragonfly42.com`-tagged ones land
in one `EVALSHA` call, `*.com` and `*` each get their own (tld tag, global tag). Three
`EVALSHA`s, one pipeline, one round trip.

For the `dragonfly42.com` group, the relevant call is roughly:

```
EVALSHA <sha> 4 \
  r:{bench:dragonfly42.com}:probe.host7.corp3.dragonfly42.com:443 \
  r:{bench:dragonfly42.com}:*.host7.corp3.dragonfly42.com:443 \
  r:{bench:dragonfly42.com}:*.corp3.dragonfly42.com:443 \
  r:{bench:dragonfly42.com}:*.dragonfly42.com:443 \
  0,0,1,1 1,0,0,0 0,0,1,1,0 \
  probe.dest42.example.com *.dest42.example.com *.example.com *.com *
```

#### What those three comma-lists are

Redis/Lua scripts only take flat strings as arguments — no structs, no per-key
booleans. But the script still needs to know, for every KEY and every field it's
about to check, "did reaching this candidate require going more than one level
deep" and "is this the literal exact-match candidate." Rather than have the *Lua*
side re-derive that from the pattern text, the Go client works it out once — it
already built these candidate lists to send them — and ships the answer as three
cheap comma-joined `0`/`1` strings, one flag per position in the list it lines up
with:

- `0,0,1,1` (ARGV[1], `kmulti`) — one flag per KEY, same order as the KEYS list:
  "did *this* fqdn1 candidate need a multi-level wildcard to reach?" The exact host
  and its immediate `*.host7...` parent are `0` (no wildcard, or just one label
  substituted); the two deeper ancestors are `1`.
- `1,0,0,0` (ARGV[2], `kexact`) — one flag per KEY again: "is this the literal,
  no-wildcard, rank-0 candidate?" Only the very first key (the exact host) is `1`.
- `0,0,1,1,0` (ARGV[3], `fmulti`) — same idea, one flag per *field* (fqdn2
  candidate) instead of per key.

Inside the script, `bits()` splits each string back into a Lua array of booleans, so
`kmulti[i]`, `kexact[i]`, `fmulti[j]` line up positionally with `KEYS[i]` and
`fields[j]`. For any hit found at key `i`, field `j`:

```lua
local exact = kexact[i] and (j == 1)
local deep  = kmulti[i] or fmulti[j]
```

`exact` means "both sides matched their literal string, no wildcard anywhere" — an
exact hit is never scope-filtered, whatever scope the rule declared. `deep` means
"at least one side only matched because a wildcard reached down more than one
label." That's the thing a rule's stored scope (`S`/`M`/`B`) restricts. The whole
reason these flags exist: so the *script* never has to re-parse
`"*.corp3.dragonfly42.com"` to work out how many labels a wildcard reaches — the
client already knows, since it built the pattern in the first place, so it just
ships the answer alongside it.

Inside, the script does an `HMGET` per key against all 5 `fqdn2` candidates. Three of
the four keys don't exist. The second key (our key with 2 rules) hits on field
`*.dest42.example.com` → value `"Brule:42"`. It's not scope-blocked, so the script
returns immediately:

```
{ 2, 2, "rule:42" }     -- 2nd key tried, 2nd field tried, rule id "rule:42"
```

The `*.com` (tld) and `*` (global) groups both come back `nil` — no rules registered
there.

**What the Go client ends up with:**

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

That's the whole payload for one lookup: a rule ID plus which two patterns matched —
not the rule's data, not a list of candidates, just the single winning pointer.

### How "most specific wins" actually works

Every candidate — fqdn1 side or fqdn2 side — gets a `Rank`, assigned once in Go when
the candidate list is built, and it never changes: `0` for a literal exact match,
`1` for "one wildcard label substituted" (`*.host7.corp3.dragonfly42.com`), then
`2, 3, 4, ...` for wildcards reaching progressively further up the domain (only
possible in multi-level mode) — and the bare `*` always gets parked at a fixed rank
of `255`, no matter how many labels the query has, so it's guaranteed to be the
least specific candidate there is. Lower rank = more specific = wins.

Because the candidate lists are built in ascending-rank order *before* they're sent,
and the script's inner loop walks `KEYS`/`fields` in that same order and returns on
the very first unblocked hit, each `EVALSHA` call's result is already the
most-specific hit *within that one hash-tag group* — no extra ranking math needed
inside Lua.

The catch: one lookup can fire up to three of these calls (own-domain group,
`.com`-tld group, global group), each blind to the other two. So the Go client
still has to pick a winner across calls — that's `moreSpecific()`:

```go
func moreSpecific(rank1, rank2, bestRank1, bestRank2 int) bool {
    if rank1 != bestRank1 {
        return rank1 < bestRank1
    }
    return rank2 < bestRank2
}
```

fqdn1 specificity always dominates: a hit with a more specific *host* pattern beats
one with a more specific *destination* pattern. Rank2 (the fqdn2 side) only breaks a
tie when two hits land on the exact same fqdn1 rank. In the example above, only the
`dragonfly42.com` group returned a hit at all, so there was nothing to compare
against — it won by default with `Rank1: 1, Rank2: 1`. If the global group had *also*
matched something, it would have lost automatically: its fqdn1 rank is always `255`
(the bare `*`), which can never beat `1`.
