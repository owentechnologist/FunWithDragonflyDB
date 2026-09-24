"""Pure domain-matching logic for the FQDN-pair router: no Redis, no I/O.

A concrete query fqdn is matched against registered patterns by walking its
own ancestor chain -- from the exact literal down to the bare "*" tier --
and checking, at each level, whether a rule was registered there. Two
matching semantics share this exact walk:

  SINGLE_LEVEL (RFC 6125 / TLS-SNI convention): "*.corp.dragonfly.com"
  matches only "console.corp.dragonfly.com" (exactly one label
  substituted), not "a.console.corp.dragonfly.com".

  MULTI_LEVEL: "*.corp.dragonfly.com" matches any descendant at any depth
  below corp.dragonfly.com, with the deepest registered ancestor winning.

SINGLE_LEVEL's candidate list is always a strict subsequence of MULTI_LEVEL's
for the same fqdn, so both modes share one storage layout, one write path,
and one lookup script (see fqdn_router.py) -- mode only changes which
candidates get generated here.

Every candidate also carries a `tag`: which of three co-location groups its
Redis key belongs to (its own registrable domain, a bare-TLD tier, or the
global tier). Grouping by tag is what keeps a lookup's declared keys inside
at most 3 Dragonfly/Redis-Cluster slots regardless of how many ancestor
levels get checked.

The bare-TLD and global tiers resolve to one single key for every query, so
they can additionally be spread across `num_buckets` physical replicas. A
write fans out to all of them and a lookup picks one by hashing the query
fqdn, so those two tiers stop concentrating every lookup onto one key while
a lookup still declares at most 3 groups.
"""
from __future__ import annotations

import zlib
from dataclasses import dataclass
from enum import StrEnum
from typing import Final

MAX_LABELS: Final = 20  # DNS allows up to 127; no real registered domain needs this many.
GLOBAL_RANK: Final = 255  # the bare "*" tier: admissible under both modes (see below).


class InvalidFqdn(ValueError):
    pass


class MatchMode(StrEnum):
    SINGLE_LEVEL = "single"  # RFC 6125 / TLS-SNI: exactly one label substituted
    MULTI_LEVEL = "multi"    # any depth below the wildcard anchor


class WildcardScope(StrEnum):
    """Per-rule declaration of how deep this rule's own wildcard may reach.

    Applies only to a rule reached via a wildcard candidate; the exact
    (rank 0) candidate is never scope-filtered.
    """
    SINGLE = "S"  # this rule never matches via a rank >= 2 ("deep") candidate
    MULTI = "M"   # this rule ONLY matches via a rank >= 2 ("deep") candidate
    BOTH = "B"    # default: reachable at any rank the query's mode allows


@dataclass(frozen=True, slots=True)
class Candidate:
    """One pattern string a concrete fqdn could be matched by, plus its
    precedence rank and which hash-tag group its Redis key belongs to.

    Invariant: within a list returned by `candidates()`, rank is strictly
    increasing and index order IS precedence order (most specific first).
    """
    pattern: str
    rank: int
    needs_multi: bool  # True iff only reachable under MatchMode.MULTI_LEVEL
    tag: str


def normalize_fqdn(fqdn: str) -> str:
    """Lowercase, strip a trailing dot. Raise InvalidFqdn on '*', an empty
    label, or a label count over MAX_LABELS -- a validated boundary reject,
    not a silent truncation that could turn into a false-negative match.
    """
    if not fqdn:
        raise InvalidFqdn("empty fqdn")
    if fqdn == "*":
        raise InvalidFqdn("'*' is a pattern token, not a concrete fqdn to look up")
    normalized = fqdn.strip().rstrip(".").lower()
    if "*" in normalized:
        raise InvalidFqdn(f"concrete fqdn cannot contain '*': {fqdn!r}")
    labels = normalized.split(".")
    if any(not label for label in labels):
        raise InvalidFqdn(f"empty label in {fqdn!r}")
    if len(labels) > MAX_LABELS:
        raise InvalidFqdn(f"{fqdn!r} has {len(labels)} labels, exceeds MAX_LABELS={MAX_LABELS}")
    return normalized


def hash_tag(pattern: str) -> str:
    """Which of the 3 co-location groups a pattern's Redis key belongs to.

    Works on both a stored pattern (exact or "*."-prefixed) and a generated
    lookup candidate -- same rule either way, single source of truth:

      >=2 labels remaining -> that domain's own registrable-domain apex
       ==1 label remaining -> the bare-TLD tier for that one label
       the literal "*"      -> the global tier

    Same public-suffix caveat as any last-two-labels heuristic: wrong for a
    two-label suffix like .co.uk without a real public-suffix list.
    """
    if pattern == "*":
        return "__global__"
    remaining = pattern[2:].split(".") if pattern.startswith("*.") else pattern.split(".")
    if len(remaining) == 1:
        return f"__tld__:{remaining[0]}"
    return f"{remaining[-2]}.{remaining[-1]}"


def is_bucketed_tag(tag: str) -> bool:
    """True for the global and per-TLD tiers -- the tiers checked on every
    single lookup regardless of which domain is being queried, and so the
    ones that need bucketed replication to avoid concentrating all lookup
    traffic onto one physical key/shard. The apex tier (anything else
    hash_tag() returns) already spreads naturally across many keys, one per
    distinct registrable domain, and must never be bucketed."""
    return tag == "__global__" or tag.startswith("__tld__:")


def bucketed_tag(base_tag: str, num_buckets: int, bucket_index: int) -> str:
    """The concrete physical tag for one replica of a bucketed base tag.
    num_buckets <= 1 returns base_tag unchanged (no ":0" suffix), so the
    on-disk key format is byte-for-byte identical to the pre-bucketing
    code whenever bucketing is disabled."""
    if num_buckets <= 1:
        return base_tag
    return f"{base_tag}:{bucket_index}"


def _candidate(
    pattern: str,
    rank: int,
    needs_multi: bool,
    *,
    bucket_index: int = 0,
    num_buckets: int = 1,
) -> Candidate:
    tag = hash_tag(pattern)
    if is_bucketed_tag(tag):
        tag = bucketed_tag(tag, num_buckets, bucket_index)
    return Candidate(pattern=pattern, rank=rank, needs_multi=needs_multi, tag=tag)


def candidates(fqdn: str, mode: MatchMode, num_buckets: int = 1) -> list[Candidate]:
    """Every pattern that could match `fqdn` under `mode`, most specific first.

    SINGLE_LEVEL -> [exact, "*."+parent, "*"]                          (<= 3)
    MULTI_LEVEL  -> [exact, "*."+parent, "*."+grandparent, ..., "*"]   (<= MAX_LABELS+2)

    `fqdn` must already be normalize_fqdn()-clean. SINGLE_LEVEL's output is
    always a subsequence of MULTI_LEVEL's output for the same fqdn -- the
    two modes are one mechanism, not two.

    With `num_buckets` > 1 the bucketed tiers resolve to one replica chosen
    by hashing `fqdn` itself. Their patterns ("*", "*.com") are the same
    fixed string for every query, so only the query can spread the reads.
    """
    labels = fqdn.split(".")
    n = len(labels)
    bucket_index = zlib.crc32(fqdn.encode()) % num_buckets if num_buckets > 1 else 0
    result = [
        _candidate(fqdn, rank=0, needs_multi=False, bucket_index=bucket_index, num_buckets=num_buckets)
    ]

    if n >= 2:
        parent = ".".join(labels[1:])
        result.append(
            _candidate(
                f"*.{parent}", rank=1, needs_multi=False, bucket_index=bucket_index, num_buckets=num_buckets
            )
        )

    if mode == MatchMode.MULTI_LEVEL:
        for k in range(2, n):
            remaining = labels[k:]
            result.append(
                _candidate(
                    f"*.{'.'.join(remaining)}",
                    rank=k,
                    needs_multi=True,
                    bucket_index=bucket_index,
                    num_buckets=num_buckets,
                )
            )

    result.append(
        _candidate("*", rank=GLOBAL_RANK, needs_multi=False, bucket_index=bucket_index, num_buckets=num_buckets)
    )
    return result


@dataclass(frozen=True, slots=True)
class FieldCandidate:
    """One pattern string a concrete fqdn could be matched by, on the side
    that is a hash field, never a Redis key -- no tag/bucket data, since
    there is nothing to hash-tag or bucket for a value that's never a key.

    Same rank/precedence semantics as Candidate; index 0 is always the exact
    (rank 0) candidate, since this list is never grouped or reordered."""
    pattern: str
    rank: int
    needs_multi: bool


def field_candidates(fqdn: str, mode: MatchMode) -> list[FieldCandidate]:
    """candidates() without the hash-tag/bucket-index computation, for the
    fqdn2/field side, where that computation would be discarded unused."""
    labels = fqdn.split(".")
    n = len(labels)
    result = [FieldCandidate(pattern=fqdn, rank=0, needs_multi=False)]

    if n >= 2:
        parent = ".".join(labels[1:])
        result.append(FieldCandidate(pattern=f"*.{parent}", rank=1, needs_multi=False))

    if mode == MatchMode.MULTI_LEVEL:
        for k in range(2, n):
            remaining = labels[k:]
            result.append(
                FieldCandidate(pattern=f"*.{'.'.join(remaining)}", rank=k, needs_multi=True)
            )

    result.append(FieldCandidate(pattern="*", rank=GLOBAL_RANK, needs_multi=False))
    return result
