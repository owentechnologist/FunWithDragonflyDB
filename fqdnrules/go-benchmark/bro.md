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
  0,0,1,1 0,0,1,1 \
  probe.dest42.example.com *.dest42.example.com *.example.com *.com *
```

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
