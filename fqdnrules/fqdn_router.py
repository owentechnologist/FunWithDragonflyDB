"""Redis/Dragonfly-backed 3-way (fqdn1, port, fqdn2) rule matcher.

Storage: one Redis HASH per (fqdn1 pattern, port), hash-tagged so a lookup
touching several ancestor-candidate keys stays inside a small, bounded
number of Dragonfly/Redis-Cluster slots, never one slot per candidate:

    r:{ns:<apex>}:<fqdn1_pattern>:<port>                          -> HASH
    r:<ns>:{<token>}:<tag>:<bucket_index>:<fqdn1_pattern>:<port>  -> HASH

The apex tier (one key per registrable domain) is never bucketed. The
__tld__ and __global__ tiers are a single logical key each for every query,
so a RuleStore built with num_buckets > 1 replicates those two tiers across
that many physical keys instead, written to all of them and read from one.
<token> is a deterministic, CRC16-slot-targeted string from
fqdn_slots.compute_bucket_table(num_buckets) -- the sole hashed content, so
every replica of a given bucket index (across every tag family and
namespace) lands on the same slot. The default num_buckets=1 keeps the
__tld__/__global__ tiers on the apex-style unbucketed key form, byte for
byte.

Each hash maps fqdn2_pattern -> "<scope_char><rule_id>". fqdn2 is never a
Redis key -- it's collapsed into a hash field, so a lookup costs
|fqdn1 candidates| keys probed, not |fqdn1 candidates| x |fqdn2 candidates|.

Both MatchMode values share this exact layout and this exact script (see
fqdn_pattern.candidates() -- SINGLE_LEVEL's candidates are a subsequence of
MULTI_LEVEL's). The Lua script holds no matching policy: it only indexes
declared KEYS and filters on a scope byte it's handed; all matching
semantics live in fqdn_pattern.py, testable with zero Redis involved.

A lookup's fqdn1-side candidates are grouped by hash tag (at most 3 groups)
and dispatched as one EVALSHA per group, batched into a single pipeline --
one network round trip regardless of how many groups are touched. Each
script call only ever touches keys inside its own declared KEYS, so this
holds under Dragonfly and under real Redis Cluster slot routing alike.
"""
from __future__ import annotations

from dataclasses import dataclass

import redis

from fqdn_pattern import (
    Candidate,
    InvalidFqdn,
    MatchMode,
    WildcardScope,
    candidates,
    field_candidates,
    hash_tag,
    is_bucketed_tag,
    normalize_fqdn,
)
from fqdn_slots import compute_bucket_table

LUA_LOOKUP = """
-- KEYS[1..n]: candidate rule-hash keys for one hash-tag group, ascending specificity.
-- ARGV[1]:    comma-joined needs_multi bits for KEYS, ascending specificity (fqdn1 side)
-- ARGV[2]:    comma-joined "is the exact rank-0 candidate" bits for KEYS, same order
-- ARGV[3]:    comma-joined needs_multi bits for fields, ascending specificity (fqdn2 side)
-- ARGV[4..]:  field names (fqdn2 candidate patterns), ascending specificity -- fields[1] is
--              always the exact rank-0 fqdn2 candidate, never regrouped, so no separate
--              "is exact" bit list is needed for the field side.
local function bits(s)
    local t = {}
    for b in string.gmatch(s, "[^,]+") do
        t[#t + 1] = (b == "1")
    end
    return t
end

local kmulti = bits(ARGV[1])
local kexact = bits(ARGV[2])
local fmulti = bits(ARGV[3])
local fields = {}
for i = 4, #ARGV do
    fields[#fields + 1] = ARGV[i]
end

for i = 1, #KEYS do
    local vals = redis.call('HMGET', KEYS[i], unpack(fields))
    for j = 1, #vals do
        local v = vals[j]
        if v then
            -- A fully exact hit (both sides matched their literal, non-wildcard
            -- pattern) is never scope-filtered, regardless of the rule's own
            -- declared scope -- scope only restricts how far a rule's *wildcard*
            -- may reach, and an exact match involves no wildcard on either side.
            local exact = kexact[i] and (j == 1)
            local deep = kmulti[i] or fmulti[j]
            local scope = string.sub(v, 1, 1)
            local blocked = (not exact) and ((deep and scope == 'S') or ((not deep) and scope == 'M'))
            if not blocked then
                return { i, j, string.sub(v, 2) }
            end
        end
    end
end
return nil
"""


def _decode(value) -> str:
    return value.decode() if isinstance(value, bytes) else value


@dataclass(frozen=True, slots=True)
class Rule:
    fqdn1: str  # exact fqdn or a "*."-prefixed / bare "*" wildcard pattern
    port: int
    fqdn2: str
    rule_id: str
    scope: WildcardScope = WildcardScope.BOTH


@dataclass(frozen=True, slots=True)
class Match:
    rule_id: str
    fqdn1_pattern: str
    port: int
    fqdn2_pattern: str
    specificity: tuple[int, int]  # (fqdn1 rank, fqdn2 rank); lower is more specific


def _validate_port(port: int) -> None:
    if not (0 < port < 65536):
        raise InvalidFqdn(f"port {port} out of range 1..65535")


class RuleStore:
    def __init__(self, conn: redis.Redis, namespace: str = "default", num_buckets: int = 1):
        self._conn = conn
        self._namespace = namespace
        self._num_buckets = num_buckets
        self._sha = conn.script_load(LUA_LOOKUP)
        # Checked unconditionally, including num_buckets=1: a reader left at
        # the default against a namespace some other writer already bucketed
        # would otherwise silently miss every catch-all-tier rule instead of
        # failing loudly. This runs once per process (RuleStore construction,
        # not per lookup), so it costs nothing at steady state. Read only:
        # claiming the count here would let a pure reader pin a namespace no
        # writer has populated, so only put()/put_many() write it.
        raw = conn.hget(self._bucketing_meta_key(), "num_buckets")
        if raw is not None:
            claimed = int(_decode(raw))
            if claimed != num_buckets:
                raise ValueError(
                    f"namespace {namespace!r} holds catch-all-tier data written with "
                    f"num_buckets={claimed}, but this RuleStore asks for "
                    f"num_buckets={num_buckets}; rewrite the catch-all-tier rules under "
                    "the new count before querying, or use the count already on record"
                )

    def _key(self, tag: str, bucket_index: int, fqdn1_pattern: str, port: int) -> str:
        """Physical key text for one candidate. A bucketed tier (tag is
        __global__/__tld__:<x> and this store actually replicates, i.e.
        num_buckets > 1) hashes only the deterministic slot-targeted token
        for bucket_index -- namespace/tag/bucket_index/pattern/port all
        stay outside the braces as plain key-body text. Anything else (the
        apex tier, or a bucketable tag when num_buckets <= 1) keeps the
        original unbucketed form, byte for byte."""
        if is_bucketed_tag(tag) and self._num_buckets > 1:
            token = compute_bucket_table(self._num_buckets).token(bucket_index)
            return f"r:{self._namespace}:{{{token}}}:{tag}:{bucket_index}:{fqdn1_pattern}:{port}"
        return f"r:{{{self._namespace}:{tag}}}:{fqdn1_pattern}:{port}"

    def _bucketing_meta_key(self) -> str:
        return f"r:{{{self._namespace}:__meta__}}:bucketing"

    def _keys_for_pattern(self, pattern: str, port: int) -> list[str]:
        """Physical Redis key(s) a rule registered under `pattern`, or a lookup
        candidate matching it, maps to. An apex pattern maps to exactly one key,
        unchanged from before bucketing existed. A global/tld pattern maps to
        self._num_buckets replica keys, so put/delete can fan out to all of
        them and a lookup (via candidates(), which already picked one bucket)
        only ever needs to check one."""
        base_tag = hash_tag(pattern)
        if is_bucketed_tag(base_tag) and self._num_buckets > 1:
            return [self._key(base_tag, i, pattern, port) for i in range(self._num_buckets)]
        return [self._key(base_tag, 0, pattern, port)]

    def put(self, rule: Rule) -> None:
        """HSET the rule into its (fqdn1_pattern, port) hash. Idempotent:
        re-registering the same triple overwrites the same field with the
        same value. A bucketed fqdn1 pattern fans out to every replica, in
        one pipelined round trip rather than one per replica."""
        _validate_port(rule.port)
        keys = self._keys_for_pattern(rule.fqdn1, rule.port)
        value = rule.scope.value + rule.rule_id
        if len(keys) == 1:
            self._conn.hset(keys[0], rule.fqdn2, value)
            return
        pipe = self._conn.pipeline(transaction=False)
        for key in keys:
            pipe.hset(key, rule.fqdn2, value)
        pipe.hset(self._bucketing_meta_key(), "num_buckets", str(self._num_buckets))
        pipe.execute()

    def put_many(self, rules) -> None:
        """One pipeline for a bulk load. Order-independent and idempotent,
        so resuming a half-applied load converges to the same state."""
        pipe = self._conn.pipeline(transaction=False)
        bucketed = False
        for rule in rules:
            _validate_port(rule.port)
            keys = self._keys_for_pattern(rule.fqdn1, rule.port)
            bucketed = bucketed or len(keys) > 1
            for key in keys:
                pipe.hset(key, rule.fqdn2, rule.scope.value + rule.rule_id)
        if bucketed:
            pipe.hset(self._bucketing_meta_key(), "num_buckets", str(self._num_buckets))
        pipe.execute()

    def delete(self, fqdn1: str, port: int, fqdn2: str) -> bool:
        """HDEL. Redis/Dragonfly drop the hash when its last field goes, so
        key cleanup is automatic and delete is idempotent (a second call
        returns False). A bucketed fqdn1 pattern is cleared from every
        replica, and the result is True if the field existed in any."""
        keys = self._keys_for_pattern(fqdn1, port)
        if len(keys) == 1:
            return bool(self._conn.hdel(keys[0], fqdn2))
        pipe = self._conn.pipeline(transaction=False)
        for key in keys:
            pipe.hdel(key, fqdn2)
        return any(pipe.execute())

    def lookup(self, fqdn1: str, port: int, fqdn2: str, mode: MatchMode) -> Match | None:
        """Return the best matching rule, or None on a miss.

        Declares every key up front (grouped by hash tag, at most 3 groups)
        and dispatches one EVALSHA per group inside a single pipeline --
        one network round trip. Each group returns its own most-specific
        hit (fqdn1-major, fqdn2-minor); the overall winner is the minimum
        (fqdn1_rank, fqdn2_rank) across groups.
        """
        fqdn1 = normalize_fqdn(fqdn1)
        fqdn2 = normalize_fqdn(fqdn2)
        _validate_port(port)

        a_candidates = candidates(fqdn1, mode, num_buckets=self._num_buckets)
        b_candidates = field_candidates(fqdn2, mode)
        fields = [c.pattern for c in b_candidates]
        fmulti = ",".join("1" if c.needs_multi else "0" for c in b_candidates)

        groups: dict[str, list[Candidate]] = {}
        for c in a_candidates:
            groups.setdefault(c.tag, []).append(c)

        tags = list(groups.keys())
        pipe = self._conn.pipeline(transaction=False)
        for tag in tags:
            group = groups[tag]
            keys = [self._key(tag, c.bucket_index, c.pattern, port) for c in group]
            kmulti = ",".join("1" if c.needs_multi else "0" for c in group)
            kexact = ",".join("1" if c.rank == 0 else "0" for c in group)
            pipe.evalsha(self._sha, len(keys), *keys, kmulti, kexact, fmulti, *fields)
        raw_results = pipe.execute()

        best: tuple[int, int, str, str, str] | None = None
        for tag, raw in zip(tags, raw_results):
            if raw is None:
                continue
            i, j, rule_id = raw
            a_cand = groups[tag][i - 1]
            b_cand = b_candidates[j - 1]
            candidate_best = (a_cand.rank, b_cand.rank, a_cand.pattern, b_cand.pattern, _decode(rule_id))
            if best is None or candidate_best[:2] < best[:2]:
                best = candidate_best

        if best is None:
            return None
        a_rank, b_rank, a_pattern, b_pattern, rule_id = best
        return Match(
            rule_id=rule_id,
            fqdn1_pattern=a_pattern,
            port=port,
            fqdn2_pattern=b_pattern,
            specificity=(a_rank, b_rank),
        )


def recommended_num_buckets(conn: redis.Redis, default: int = 16) -> int:
    """One-time sizing helper: query the server's own reported thread count
    (Dragonfly's per-process shard count) via INFO, for a caller to bake
    into its own configuration. Deliberately not called automatically by
    RuleStore.__init__ -- picking num_buckets is a deployment-time decision
    that should be made once and stay fixed until a human decides to change
    it and rewrite the catch-all tier, not silently re-derived (and
    silently drifted) on every process restart. Falls back to `default` if
    the server doesn't report thread_count (e.g. plain Redis, not
    Dragonfly)."""
    try:
        info = conn.info("server")
        return int(info.get("thread_count", default))
    except redis.exceptions.RedisError:
        return default


def _demo() -> None:
    conn = redis.Redis(host="localhost", port=6379, db=15)
    conn.flushdb()
    store = RuleStore(conn, namespace="demo")

    store.put(Rule("*.corp.dragonfly.com", 443, "retailer.com", "rule:corp-to-retailer"))
    store.put(Rule("*.corp.dragonfly.com", 443, "*.com", "rule:corp-to-any-dot-com"))
    store.put(Rule("*.dragonfly.com", 443, "*", "rule:tls-strict", scope=WildcardScope.SINGLE))

    cases = [
        ("console.corp.dragonfly.com", 443, "retailer.com", MatchMode.SINGLE_LEVEL, "single-level exact win over wildcard"),
        ("console.corp.dragonfly.com", 443, "shop.com", MatchMode.SINGLE_LEVEL, "single-level falls through to *.com wildcard"),
        ("a.console.corp.dragonfly.com", 443, "retailer.com", MatchMode.SINGLE_LEVEL, "too deep for single-level (expected miss)"),
        ("a.console.corp.dragonfly.com", 443, "retailer.com", MatchMode.MULTI_LEVEL, "same query, multi-level mode (expected hit)"),
        ("a.b.dragonfly.com", 443, "anything.net", MatchMode.MULTI_LEVEL, "SINGLE-scoped rule invisible at deep rank (expected miss)"),
    ]
    for fqdn1, port, fqdn2, mode, label in cases:
        match = store.lookup(fqdn1, port, fqdn2, mode)
        print(f"{label:60s} -> {match}")

    conn.flushdb()


if __name__ == "__main__":
    _demo()
