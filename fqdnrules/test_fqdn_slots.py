"""Redis-free tests for fqdn_slots.py: CRC16 correctness, slot-range
partitioning, and the bucket-token search's actual placement guarantee.
"""
from __future__ import annotations

import pytest

from fqdn_slots import (
    NUM_SLOTS,
    compute_bucket_table,
    crc16,
    key_slot,
    slot,
    slot_range,
)

# ---------------------------------------------------------------------------
# CRC16 -- verify this before trusting anything built on top of it.
# ---------------------------------------------------------------------------


def test_crc16_known_answer_vector():
    assert crc16(b"123456789") == 0x31C3


def test_slot_is_crc16_masked_to_14_bits():
    assert slot(b"123456789") == 0x31C3 & 0x3FFF


def test_slot_range_of_values_stays_within_14_bits():
    for data in (b"", b"a", b"foo", b"{user:123}", b"0" * 100):
        s = slot(data)
        assert 0 <= s < NUM_SLOTS


# ---------------------------------------------------------------------------
# key_slot: hash-tag-aware extraction.
# ---------------------------------------------------------------------------


def test_key_slot_hashes_only_the_hash_tag_content():
    assert key_slot("{user:123}:profile") == slot(b"user:123")
    assert key_slot("prefix:{user:123}:suffix") == slot(b"user:123")


def test_key_slot_falls_back_to_whole_key_without_a_tag():
    assert key_slot("plain:key") == slot(b"plain:key")


def test_key_slot_falls_back_on_an_empty_hash_tag():
    assert key_slot("{}:rest") == slot(b"{}:rest")


def test_key_slot_falls_back_when_closing_brace_is_missing():
    assert key_slot("{unterminated") == slot(b"{unterminated")


# ---------------------------------------------------------------------------
# slot_range: every slot belongs to exactly one bucket's range.
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("num_buckets", [1, 2, 3, 5, 7, 8, 16, 37, 100, 129, 130, 192])
def test_slot_range_partitions_the_keyspace_exactly(num_buckets):
    ranges = [slot_range(i, num_buckets) for i in range(num_buckets)]

    assert ranges[0][0] == 0
    assert ranges[-1][1] == NUM_SLOTS
    for (lo, hi) in ranges:
        assert lo < hi  # every bucket gets at least one slot (num_buckets <= NUM_SLOTS)
    for (_, hi_prev), (lo_next, _) in zip(ranges, ranges[1:]):
        assert hi_prev == lo_next  # contiguous, no gap, no overlap

    owner = [None] * NUM_SLOTS
    for i, (lo, hi) in enumerate(ranges):
        for s in range(lo, hi):
            owner[s] = i
    assert all(o is not None for o in owner)


# ---------------------------------------------------------------------------
# compute_bucket_table: the actual placement guarantee, verified programmatically.
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("num_buckets", [1, 2, 7, 8, 12, 16, 37, 64, 128, 129, 192])
def test_bucket_table_tokens_land_in_their_own_slot_range(num_buckets):
    table = compute_bucket_table(num_buckets)
    assert len(table.tokens) == num_buckets
    assert len(table.slots) == num_buckets
    assert len(set(table.tokens)) == num_buckets  # every token distinct

    for i, token in enumerate(table.tokens):
        lo, hi = slot_range(i, num_buckets)
        computed_slot = slot(token.encode())
        assert computed_slot == table.slots[i]
        assert lo <= computed_slot < hi


def test_bucket_table_is_modulo_safe_up_to_129_and_not_above():
    assert compute_bucket_table(129).modulo_safe is True
    assert compute_bucket_table(130).modulo_safe is False


@pytest.mark.parametrize("num_buckets", [8, 37, 129])
def test_modulo_safe_tokens_also_satisfy_the_interleaved_policy(num_buckets):
    table = compute_bucket_table(num_buckets)
    assert table.modulo_safe is True
    for i, s in enumerate(table.slots):
        assert s % num_buckets == i


def test_compute_bucket_table_is_memoized():
    assert compute_bucket_table(16) is compute_bucket_table(16)


def test_token_accessor_raises_index_error_out_of_range():
    table = compute_bucket_table(4)
    with pytest.raises(IndexError):
        table.token(4)
    with pytest.raises(IndexError):
        table.token(-1)


@pytest.mark.parametrize("bad_num_buckets", [0, -1, 16385])
def test_compute_bucket_table_rejects_out_of_range_num_buckets(bad_num_buckets):
    with pytest.raises(ValueError):
        compute_bucket_table(bad_num_buckets)
