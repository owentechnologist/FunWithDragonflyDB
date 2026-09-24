# FQDN router: write path and query path, in pseudo-code

Python (`fqdn_router.py` and `fqdn_pattern.py`) and Go (`go-benchmark` and
`go-router-service`) implement the same algorithm, the same key layout, and
the same Lua script. This doc walks through that algorithm once, in
pseudo-code. Then it traces three concrete queries against one small rule
set: one resolved at the apex tier, one at the TLD tier, one at the global
tier.

Storage recap: one Redis/Dragonfly HASH per `(fqdn1_pattern, port)`, its key
hash-tagged into one of three groups:

```
r:{ns:<apex-domain>}:<fqdn1_pattern>:<port>     -- e.g. r:{ns:dragonfly.com}:*.corp.dragonfly.com:443
r:{ns:__tld__:<tld>}:<fqdn1_pattern>:<port>     -- e.g. r:{ns:__tld__:com}:*.com:443
r:{ns:__global__}:<fqdn1_pattern>:<port>        -- e.g. r:{ns:__global__}:*:443
```

Each hash maps an `fqdn2` pattern (field) to `<scope_char><rule_id>` (value).

## Write path

```
function PUT(fqdn1_pattern, port, fqdn2_pattern, rule_id, scope = BOTH):
    validate 1 <= port <= 65535

    base_tag = HASH_TAG(fqdn1_pattern)          # apex domain, "__tld__:x", or "__global__"
    if IS_BUCKETED_TAG(base_tag) and num_buckets > 1:
        keys = [ KEY(BUCKETED_TAG(base_tag, i), fqdn1_pattern, port)
                 for i in 0 .. num_buckets-1 ]   # fan out to every replica
    else:
        keys = [ KEY(base_tag, fqdn1_pattern, port) ]   # exactly one key

    value = scope.char + rule_id
    for key in keys:
        HSET(key, field = fqdn2_pattern, value = value)   # idempotent: same field, same value

    if len(keys) > 1:
        HSET(bucketing_meta_key, "num_buckets", num_buckets)   # record the fan-out width once
```

`HASH_TAG(pattern)`:

```
function HASH_TAG(pattern):
    if pattern == "*":                 return "__global__"
    labels = strip_optional_leading("*.", pattern).split(".")
    if len(labels) == 1:                return "__tld__:" + labels[0]
    return labels[-2] + "." + labels[-1]     # the pattern's own apex domain
```

Deleting a rule is `HDEL` over the same key set. Redis/Dragonfly drops a
hash automatically once its last field is removed, so key cleanup needs no
extra step.

## Query path

```
function LOOKUP(fqdn1, port, fqdn2, mode) -> Match | MISS:
    fqdn1 = NORMALIZE(fqdn1)             # lowercase, strip trailing dot, reject "*"/empty/too-deep
    fqdn2 = NORMALIZE(fqdn2)
    validate 1 <= port <= 65535

    a_candidates = CANDIDATES(fqdn1, mode)     # fqdn1 side: keys, ranked most- to least-specific
    b_candidates = FIELD_CANDIDATES(fqdn2, mode)  # fqdn2 side: hash fields, same ranking, never keys

    groups = group a_candidates by .tag         # at most 3 groups: apex, tld, or global

    # one EVALSHA per group, all dispatched in a single pipeline (one round trip)
    for tag, group in groups:
        keys   = [ KEY(tag, c.pattern, port) for c in group ]
        result[tag] = EVALSHA(LUA_LOOKUP, keys,
                               kmulti  = [c.needs_multi for c in group],
                               kexact  = [c.rank == 0   for c in group],
                               fmulti  = [c.needs_multi for c in b_candidates],
                               fields  = [c.pattern     for c in b_candidates])

    best = None
    for tag, (key_i, field_j, rule_id) in result where not MISS:
        candidate = (a_candidates[tag][key_i].rank, b_candidates[field_j].rank, ...)
        if best is None or candidate.ranks < best.ranks:
            best = candidate

    return best or MISS
```

`CANDIDATES(fqdn, mode)`: the ancestor walk, most specific first.

```
function CANDIDATES(fqdn, mode):
    labels = fqdn.split(".")
    bucket_index = crc32(fqdn) % num_buckets if num_buckets > 1 else 0   # ONE index for this whole call

    result = [ candidate(fqdn, rank=0, needs_multi=false) ]              # exact
    if len(labels) >= 2:
        result += candidate("*." + labels[1:], rank=1, needs_multi=false)   # one-label wildcard

    if mode == MULTI_LEVEL:
        for k in 2 .. len(labels)-1:
            result += candidate("*." + labels[k:], rank=k, needs_multi=true) # deeper ancestors

    result += candidate("*", rank=255, needs_multi=false)                # global, always present

    return result   # each candidate's .tag already has bucket_index applied if it's a bucketed tier
```

One detail matters: `CANDIDATES` computes `bucket_index` once per query,
from the full `fqdn1` being looked up, and reuses that value for every
bucketed candidate it produces. So if a lookup touches both the TLD tier
and the global tier, both land on the same replica index. For example,
both might resolve to replica `:2`. A lookup therefore never fans out
across bucket replicas to find a match. Only a write does that (see `PUT`
above).

Inside the Lua script (`LUA_LOOKUP`, shared byte-for-byte by both
implementations), each key's `HMGET` call fetches every `fqdn2` field
candidate at once. The script walks the results in specificity order and
returns the first present value, unless scope blocks it.
`WildcardScope.SINGLE` and `WildcardScope.MULTI` restrict how deep a rule's
own wildcard can reach. An exact match on both sides is never blocked.
Each group returns its own best `(key_rank, field_rank, rule_id)`. The
caller takes the minimum across groups.

## Three worked queries

Rule set registered ahead of time (`port = 443` throughout, `num_buckets =
1` so tier keys have no bucket suffix):

```
PUT("*.corp.dragonfly.com", 443, "retailer.com", "rule:corp-to-retailer")
PUT("*.com",                443, "*",            "rule:tld-catchall")
PUT("*",                    443, "*",            "rule:global-catchall")
```

That writes three hashes:

```
r:{ns:dragonfly.com}:*.corp.dragonfly.com:443    { retailer.com -> "Brule:corp-to-retailer" }
r:{ns:__tld__:com}:*.com:443                     { *            -> "Brule:tld-catchall" }
r:{ns:__global__}:*:443                          { *            -> "Brule:global-catchall" }
```

### Query 1: resolved at the apex tier

`LOOKUP("console.corp.dragonfly.com", 443, "retailer.com", MULTI_LEVEL)`

fqdn1 candidates, most specific first (tag shown for each):

| pattern | tag |
|---|---|
| `console.corp.dragonfly.com` | `dragonfly.com` |
| `*.corp.dragonfly.com` | `dragonfly.com` |
| `*.dragonfly.com` | `dragonfly.com` |
| `*.com` | `__tld__:com` |
| `*` | `__global__` |

All three `dragonfly.com`-tagged candidates group into one `EVALSHA` call,
which checks keys `console.corp.dragonfly.com:443`,
`*.corp.dragonfly.com:443`, and `*.dragonfly.com:443`, in that order. The
first key does not exist. The second, `*.corp.dragonfly.com:443`, does:
`HMGET` finds the field `retailer.com` there directly. The fqdn2 side
matches at rank 0. The fqdn1 side matches through the wildcard
`*.corp.dragonfly.com`, at rank 1.

The lookup dispatches the `__tld__:com` and `__global__` groups in the
same pipeline. Both also hit: each group's hash has a rule at fqdn2
pattern `*`, and `*` matches any fqdn2, including `retailer.com`. But their
fqdn1 rank is worse (3 and 255) than the apex group's rank (1), so the
apex hit wins the comparison across groups.

**Result:** `rule:corp-to-retailer` wins, matched at fqdn1 pattern
`*.corp.dragonfly.com:443` and fqdn2 pattern `retailer.com`. This is an
apex-tier hit.

### Query 2: resolved at the TLD tier

`LOOKUP("shop.unregistered-domain.com", 443, "anything.net", MULTI_LEVEL)`

fqdn1 candidates:

| pattern | tag |
|---|---|
| `shop.unregistered-domain.com` | `unregistered-domain.com` |
| `*.unregistered-domain.com` | `unregistered-domain.com` |
| `*.com` | `__tld__:com` |
| `*` | `__global__` |

Neither key in the `unregistered-domain.com` group exists: nobody ever put
a rule there. That group's `EVALSHA` call returns a miss.

The `__tld__:com` group probes `r:{ns:__tld__:com}:*.com:443`. `HMGET`
checks the fqdn2 candidates `anything.net`, `*.net`, and `*`, and finds
field `*`, the catch-all destination. The `__global__` group also hits, on
the same fqdn2 field `*` and rule `rule:global-catchall`, but its fqdn1
rank, 255, is worse than the TLD group's rank at `*.com`. The TLD hit
wins.

**Result:** `rule:tld-catchall` wins, matched at fqdn1 pattern `*.com:443`
and fqdn2 pattern `*`. This is a TLD-tier hit, for a domain that was never
individually registered.

### Query 3: resolved at the global tier

`LOOKUP("anything.totally-unrelated.org", 443, "somewhere-else.io", MULTI_LEVEL)`

fqdn1 candidates:

| pattern | tag |
|---|---|
| `anything.totally-unrelated.org` | `totally-unrelated.org` |
| `*.totally-unrelated.org` | `totally-unrelated.org` |
| `*.org` | `__tld__:org` |
| `*` | `__global__` |

Nobody ever registered a rule under `totally-unrelated.org` (an apex
miss) or under `*.org`. Only `__tld__:com` exists, left over from Query
2's setup, so the `__tld__:org` hash is missing too. Both those groups
return a miss. Only the `__global__` group has a key at all,
`r:{ns:__global__}:*:443`, and its lone field `*` matches every possible
fqdn2, including `somewhere-else.io`.

**Result:** `rule:global-catchall` wins, matched at fqdn1 pattern `*:443`
and fqdn2 pattern `*`. This is a global-tier hit: the domain's own TLD,
`.org`, has no catch-all of its own, so the lookup falls through to the
tier that answers for every domain and every TLD.

## What changes if `num_buckets > 1`

Only the TLD and global key names change. Each becomes `N` replica keys,
for example `__global__:0` through `__global__:N-1`. The apex tier is
never bucketed. `PUT` writes the rule to all `N` replicas. A `LOOKUP`
still computes one `bucket_index` from the query's own `fqdn1` and reads
only the one replica that index selects. Query 2 and Query 3 above would
each touch a single, different `__tld__:com:i` or `__global__:j` key
instead of the unsuffixed name, never more than one per tier.
