"""Deterministic Redis-Cluster-slot-targeted bucket tokens: no Redis, no I/O.

The `__global__` and `__tld__:<tld>` tiers in fqdn_router.py replicate across
`num_buckets` physical keys so lookups spread across worker threads instead
of hammering one key. This module computes, for a given `num_buckets`, N
distinct opaque string tokens such that token i's CRC16 hash slot falls
inside thread i's exclusive, equal-sized share of the 16384-slot Redis
Cluster keyspace -- a guarantee, not a coincidence of whatever slot a
literal `"<tag>:<i>"` string happened to land in.

The token table is a pure function of `num_buckets` alone (see
compute_bucket_table), shared by every namespace and every tag family, and
is never persisted: every process recomputes the identical table.

Layering: CRC16/slot/token-search logic lives here, not in fqdn_pattern.py
(which stays "no Redis, no I/O" and only decides *which* bucket a query
belongs to) or in fqdn_router.py (which only asks this module for a token
and builds key strings with it).
"""
from __future__ import annotations

import bisect
import functools
import json
import math
import os
import sys
from dataclasses import dataclass

NUM_SLOTS = 16384  # Redis Cluster's fixed hash-slot space.


def crc16(data: bytes) -> int:
    """CRC-16/XMODEM: poly 0x1021, init 0x0000, no reflection, no final XOR.

    This is the exact variant Redis Cluster's HASH_SLOT uses (easily
    confused with CRC-16/CCITT-FALSE, which differs only in init=0xFFFF).
    Known-answer vector: crc16(b"123456789") == 0x31C3.
    """
    crc = 0x0000
    for byte in data:
        crc ^= byte << 8
        for _ in range(8):
            if crc & 0x8000:
                crc = ((crc << 1) ^ 0x1021) & 0xFFFF
            else:
                crc = (crc << 1) & 0xFFFF
    return crc


def slot(data: bytes) -> int:
    """The 0..16383 Redis Cluster hash slot for raw bytes."""
    return crc16(data) & 0x3FFF


def key_slot(key: str) -> int:
    """Redis-Cluster hash-tag-aware slot for a full key string.

    Hashes only the content of the first non-empty `{...}` span if one is
    present (matching Redis's own hash-tag extraction); otherwise hashes
    the whole key. Standalone utility for the diagnose_*.py scripts, which
    otherwise have no way to compute a slot at all.
    """
    start = key.find("{")
    if start != -1:
        end = key.find("}", start + 1)
        if end != -1 and end != start + 1:
            return slot(key[start + 1 : end].encode())
    return slot(key.encode())


def slot_range(i: int, num_buckets: int) -> tuple[int, int]:
    """Bucket i's half-open share of the slot space: [lo, hi).

    Floor-boundary partition: boundaries[i] = floor(i * 16384 / N). Range
    widths differ by at most 1 slot; which buckets get the extra slot falls
    out of the floor arithmetic rather than an explicit remainder branch.
    """
    lo = (i * NUM_SLOTS) // num_buckets
    hi = ((i + 1) * NUM_SLOTS) // num_buckets
    return lo, hi


def _bucket_has_modulo_match(lo: int, hi: int, num_buckets: int, bucket_index: int) -> bool:
    """True iff [lo, hi) contains a slot s with s % num_buckets == bucket_index."""
    smallest = lo + (bucket_index - lo) % num_buckets
    return smallest < hi


def _is_modulo_safe(num_buckets: int) -> bool:
    """True iff every bucket's contiguous range also contains a slot that
    satisfies the interleaved `slot % num_buckets == bucket_index` policy --
    pure arithmetic, independent of CRC16. Empirically true for every
    1 <= N <= 129 and for N in {16383, 16384} (each bucket's range there is
    so narrow, 1-2 slots, that it trivially contains its own modulo residue);
    false for every 130 <= N <= 16382. Verified by direct enumeration over
    the full legal range, not assumed -- see the synthesis doc's ~129/~192
    estimates, which this recomputes rather than trusts.
    """
    return all(
        _bucket_has_modulo_match(*slot_range(i, num_buckets), num_buckets, i)
        for i in range(num_buckets)
    )


def _iteration_cap(num_buckets: int) -> int:
    """Generous ceiling on candidate draws before compute_bucket_table gives
    up loudly instead of spinning forever.

    Filling N bins by uniform random draws is coupon-collector: expected
    draws ~= N * ln(N). The modulo-safe search additionally requires
    `slot % N == bucket_index`, roughly halving each draw's acceptance
    chance for its bucket, so budget a large constant-factor margin over
    the plain estimate rather than model the dual-constraint expectation
    exactly. 200x plus a flat floor comfortably covers both the modulo-safe
    and contiguous-only cases at every legal N, including N close to 16384.
    """
    return 200 * num_buckets * math.ceil(math.log(max(num_buckets, 2))) + 10_000


@dataclass(frozen=True, slots=True)
class BucketTable:
    """The deterministic token table for one num_buckets value.

    tokens[i] is the hash-tag content whose CRC16 slot (slots[i]) falls
    inside bucket i's slot_range(i, len(tokens)). modulo_safe records
    whether every token additionally satisfies slot % num_buckets == i,
    the interleaved policy Redis Cluster itself uses across real nodes --
    useful if Dragonfly's internal thread-sharding turns out to follow that
    scheme rather than contiguous blocks.
    """

    tokens: tuple[str, ...]
    slots: tuple[int, ...]
    modulo_safe: bool

    def token(self, i: int) -> str:
        if not (0 <= i < len(self.tokens)):
            raise IndexError(f"bucket index {i} out of range for a table of size {len(self.tokens)}")
        return self.tokens[i]


@functools.lru_cache(maxsize=None)
def compute_bucket_table(num_buckets: int) -> BucketTable:
    """Compute the token table for num_buckets, memoized per process.

    Single ascending-counter, first-fit-per-bucket coupon-collector search.
    Candidates are the decimal digits of an increasing counter ("0", "1",
    "2", ...) -- deterministic, seedless, and identical in every language
    and process for the same num_buckets, which is what lets a writer and a
    reader agree on key names with no coordination channel between them.

    This candidate order is a frozen contract once shipped: changing it
    (or the CRC16 algorithm) silently produces a different table for the
    same num_buckets and must not happen without a coordinated rewrite of
    every deployed namespace's bucketed tiers.
    """
    if not (1 <= num_buckets <= 16384):
        raise ValueError(f"num_buckets must be in [1, 16384], got {num_buckets}")

    ranges = [slot_range(i, num_buckets) for i in range(num_buckets)]
    starts = [lo for lo, _hi in ranges]
    modulo_safe = _is_modulo_safe(num_buckets)

    tokens: list[str | None] = [None] * num_buckets
    slots: list[int | None] = [None] * num_buckets
    filled = 0
    cap = _iteration_cap(num_buckets)
    k = 0
    while filled < num_buckets:
        if k > cap:
            raise RuntimeError(
                f"compute_bucket_table(num_buckets={num_buckets}): could not fill all "
                f"buckets within {cap} candidate draws; CRC16 may be broken, or this "
                "num_buckets is pathological"
            )
        candidate = str(k)
        s = slot(candidate.encode())
        bucket = bisect.bisect_right(starts, s) - 1
        if tokens[bucket] is None and (not modulo_safe or s % num_buckets == bucket):
            tokens[bucket] = candidate
            slots[bucket] = s
            filled += 1
        k += 1

    return BucketTable(tokens=tuple(tokens), slots=tuple(slots), modulo_safe=modulo_safe)


_GOLDEN_NUM_BUCKETS = (1, 2, 7, 8, 12, 16, 37, 64, 128, 129, 192)


def _emit_golden(path: str) -> None:
    known_answer = crc16(b"123456789")
    tables = {}
    for n in _GOLDEN_NUM_BUCKETS:
        table = compute_bucket_table(n)
        tables[str(n)] = {
            "tokens": list(table.tokens),
            "slots": list(table.slots),
            "modulo_safe": table.modulo_safe,
        }
    payload = {
        "crc16_known_answer": {
            "input": "123456789",
            "crc16": known_answer,
            "crc16_hex": f"0x{known_answer:04X}",
        },
        "num_buckets": tables,
    }
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        json.dump(payload, f, indent=2, sort_keys=True)
        f.write("\n")


if __name__ == "__main__":
    if len(sys.argv) == 2 and sys.argv[1] == "--emit-golden":
        _out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "testdata", "slot_tokens_golden.json")
        _emit_golden(_out)
        print(f"wrote {_out}")
    else:
        raise SystemExit("usage: python fqdn_slots.py --emit-golden")
