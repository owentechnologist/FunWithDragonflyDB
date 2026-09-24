"""Correctness and scaling tests for the two-sided wildcard FQDN-pair router.

Requires a real Redis/Dragonfly reachable at localhost:6379 (EVALSHA isn't
meaningfully fakeable). Everything runs against db=15 and flushes it before
and after so it never touches real data.
"""
from __future__ import annotations

import statistics
import time

import pytest
import redis

from fqdn_pattern import GLOBAL_RANK, InvalidFqdn, MatchMode, WildcardScope, candidates
from fqdn_router import Rule, RuleStore
from fqdn_slots import compute_bucket_table

TEST_DB = 15


@pytest.fixture
def conn():
    try:
        c = redis.Redis(host="localhost", port=6379, db=TEST_DB)
        c.ping()
    except redis.exceptions.ConnectionError:
        pytest.skip("no local redis/dragonfly reachable on localhost:6379")
    c.flushdb()
    yield c
    c.flushdb()


@pytest.fixture
def store(conn):
    return RuleStore(conn, namespace="test")


# ---------------------------------------------------------------------------
# Pure candidate-generation properties (fqdn_pattern.py, no Redis at all)
# ---------------------------------------------------------------------------


def test_single_level_candidates_are_a_subsequence_of_multi_level():
    fqdn = "a.b.c.example.com"
    single = [c.pattern for c in candidates(fqdn, MatchMode.SINGLE_LEVEL)]
    multi = [c.pattern for c in candidates(fqdn, MatchMode.MULTI_LEVEL)]
    it = iter(multi)
    assert all(p in it for p in single), f"{single} is not a subsequence of {multi}"


def test_candidate_ranks_strictly_increase():
    ranks = [c.rank for c in candidates("a.b.c.example.com", MatchMode.MULTI_LEVEL)]
    assert ranks == sorted(ranks) and len(ranks) == len(set(ranks))


def test_global_tier_always_present_and_not_deep():
    for mode in (MatchMode.SINGLE_LEVEL, MatchMode.MULTI_LEVEL):
        cs = candidates("example.com", mode)
        assert cs[-1].pattern == "*"
        assert cs[-1].rank == GLOBAL_RANK
        assert cs[-1].needs_multi is False


def test_lookup_rejects_a_bare_wildcard_as_a_concrete_query(store):
    with pytest.raises(InvalidFqdn):
        store.lookup("*", 443, "backend.com", MatchMode.MULTI_LEVEL)


def test_max_labels_boundary_rejects_rather_than_truncates(store):
    too_deep = ".".join(f"l{i}" for i in range(25)) + ".com"
    with pytest.raises(InvalidFqdn):
        store.lookup(too_deep, 443, "backend.com", MatchMode.MULTI_LEVEL)


# ---------------------------------------------------------------------------
# End-to-end matching semantics
# ---------------------------------------------------------------------------


def test_exact_match_both_modes(store):
    store.put(Rule("api.foo.com", 443, "backend-api.foo.com", "r-exact"))
    for mode in (MatchMode.SINGLE_LEVEL, MatchMode.MULTI_LEVEL):
        m = store.lookup("api.foo.com", 443, "backend-api.foo.com", mode)
        assert m is not None and m.rule_id == "r-exact" and m.specificity == (0, 0)


def test_miss_on_wrong_fqdn2_and_wrong_port(store):
    store.put(Rule("api.foo.com", 443, "backend-api.foo.com", "r-exact"))
    assert store.lookup("api.foo.com", 443, "wrong.foo.com", MatchMode.MULTI_LEVEL) is None
    assert store.lookup("api.foo.com", 8080, "backend-api.foo.com", MatchMode.MULTI_LEVEL) is None


def test_worked_example_exact_fqdn2_beats_wildcard_fqdn2(store):
    """The scenario from the spec: *.corp.dragonfly.com carries two fqdn2
    rules (exact retailer.com, wildcard *.com); a concrete query matching
    both must prefer the exact one."""
    store.put(Rule("*.corp.dragonfly.com", 443, "retailer.com", "r-exact-dest"))
    store.put(Rule("*.corp.dragonfly.com", 443, "*.com", "r-wild-dest"))

    m = store.lookup("console.corp.dragonfly.com", 443, "retailer.com", MatchMode.SINGLE_LEVEL)
    assert m.rule_id == "r-exact-dest"
    assert m.fqdn1_pattern == "*.corp.dragonfly.com"
    assert m.fqdn2_pattern == "retailer.com"

    # A destination that only satisfies the wildcard rule still matches.
    m2 = store.lookup("console.corp.dragonfly.com", 443, "shop.com", MatchMode.SINGLE_LEVEL)
    assert m2.rule_id == "r-wild-dest"
    assert m2.fqdn2_pattern == "*.com"


def test_single_level_rejects_descendant_two_levels_down(store):
    store.put(Rule("*.corp.dragonfly.com", 443, "retailer.com", "r-corp"))
    assert store.lookup("a.console.corp.dragonfly.com", 443, "retailer.com", MatchMode.SINGLE_LEVEL) is None


def test_multi_level_accepts_descendant_two_levels_down(store):
    store.put(Rule("*.corp.dragonfly.com", 443, "retailer.com", "r-corp"))
    m = store.lookup("a.console.corp.dragonfly.com", 443, "retailer.com", MatchMode.MULTI_LEVEL)
    assert m is not None and m.rule_id == "r-corp"
    assert m.specificity[0] == 2  # two labels stripped to reach the registered anchor


def test_multi_level_longest_match_wins_over_broader_ancestor(store):
    store.put(Rule("*.dragonfly.com", 443, "retailer.com", "r-broad"))
    store.put(Rule("*.corp.dragonfly.com", 443, "retailer.com", "r-narrow"))
    m = store.lookup("console.corp.dragonfly.com", 443, "retailer.com", MatchMode.MULTI_LEVEL)
    assert m.rule_id == "r-narrow"


def test_global_fallback_reached_only_when_nothing_more_specific_matches(store):
    store.put(Rule("api.foo.com", 443, "backend.foo.com", "r-specific"))
    store.put(Rule("*", 443, "catch-all.example.com", "r-global"))

    m1 = store.lookup("api.foo.com", 443, "backend.foo.com", MatchMode.MULTI_LEVEL)
    assert m1.rule_id == "r-specific"

    m2 = store.lookup("api.foo.com", 443, "catch-all.example.com", MatchMode.MULTI_LEVEL)
    assert m2.rule_id == "r-global" and m2.fqdn1_pattern == "*"


def test_single_scope_rule_invisible_at_deep_rank_but_visible_shallow(store):
    store.put(Rule("*.dragonfly.com", 443, "*", "r-tls-strict", scope=WildcardScope.SINGLE))

    # Rank 1 (direct child): visible.
    shallow = store.lookup("host1.dragonfly.com", 443, "anything.net", MatchMode.MULTI_LEVEL)
    assert shallow is not None and shallow.rule_id == "r-tls-strict"

    # Rank 2+ (grandchild): the SINGLE scope blocks it even in MULTI_LEVEL mode.
    deep = store.lookup("a.b.dragonfly.com", 443, "anything.net", MatchMode.MULTI_LEVEL)
    assert deep is None


def test_multi_scope_rule_invisible_at_shallow_rank_but_visible_deep(store):
    store.put(Rule("*.dragonfly.com", 443, "*", "r-deep-only", scope=WildcardScope.MULTI))

    shallow = store.lookup("host1.dragonfly.com", 443, "anything.net", MatchMode.MULTI_LEVEL)
    assert shallow is None  # rank 1 is not "deep"; MULTI scope excludes it

    deep = store.lookup("a.b.dragonfly.com", 443, "anything.net", MatchMode.MULTI_LEVEL)
    assert deep is not None and deep.rule_id == "r-deep-only"


def test_exact_match_is_never_scope_filtered(store):
    store.put(Rule("host9.gamma.io", 8443, "backend9.gamma.io", "r-exact-multi-scope", scope=WildcardScope.MULTI))
    hit = store.lookup("host9.gamma.io", 8443, "backend9.gamma.io", MatchMode.MULTI_LEVEL)
    assert hit is not None
    assert hit.rule_id == "r-exact-multi-scope"
    assert hit.specificity == (0, 0)


def test_shards_isolate_domains_with_identical_rule_shapes(store):
    store.put(Rule("api.alpha.com", 443, "backend-api.alpha.com", "r-shared-id"))
    store.put(Rule("api.beta.com", 443, "backend-api.beta.com", "r-shared-id"))
    assert store.lookup("api.alpha.com", 443, "backend-api.beta.com", MatchMode.MULTI_LEVEL) is None
    assert store.lookup("api.beta.com", 443, "backend-api.alpha.com", MatchMode.MULTI_LEVEL) is None


def test_delete_is_idempotent(store):
    store.put(Rule("api.foo.com", 443, "backend.foo.com", "r-x"))
    assert store.delete("api.foo.com", 443, "backend.foo.com") is True
    assert store.delete("api.foo.com", 443, "backend.foo.com") is False
    assert store.lookup("api.foo.com", 443, "backend.foo.com", MatchMode.MULTI_LEVEL) is None


def test_put_is_idempotent_on_replay(store):
    rule = Rule("api.foo.com", 443, "backend.foo.com", "r-x")
    store.put(rule)
    store.put(rule)  # replaying an insert must not create a second entry / error
    m = store.lookup("api.foo.com", 443, "backend.foo.com", MatchMode.MULTI_LEVEL)
    assert m.rule_id == "r-x"


# ---------------------------------------------------------------------------
# Bucketed replication of the global / per-TLD tiers
# ---------------------------------------------------------------------------

# Chosen so crc32(fqdn) % 4 covers all four buckets and % 8 covers five of eight.
BUCKET_PROBE_FQDNS = [
    "shop.omega.com",
    "api.omega.net",
    "www.kappa.org",
    "portal.sigma.dev",
    "mail.lambda.io",
]


def test_default_store_writes_the_unbucketed_key_name_and_still_matches(conn):
    store = RuleStore(conn, namespace="test")
    store.put(Rule("api.foo.com", 443, "backend-api.foo.com", "r-exact"))

    assert conn.keys("r:{test:*") == [b"r:{test:foo.com}:api.foo.com:443"]
    for mode in (MatchMode.SINGLE_LEVEL, MatchMode.MULTI_LEVEL):
        m = store.lookup("api.foo.com", 443, "backend-api.foo.com", mode)
        assert m is not None and m.rule_id == "r-exact" and m.specificity == (0, 0)


def test_bucketed_put_writes_the_global_rule_into_four_replica_keys(conn):
    store = RuleStore(conn, namespace="buckets", num_buckets=4)
    store.put(Rule(fqdn1="*", port=443, fqdn2="*", rule_id="rule:global"))

    table = compute_bucket_table(4)
    expected = sorted(f"r:buckets:{{{table.token(i)}}}:__global__:{i}:*:443" for i in range(4))
    assert sorted(k.decode() for k in conn.keys("r:buckets:*")) == expected
    for key in expected:
        assert conn.hget(key, "*") == b"Brule:global"

    # The apex tier already spreads over one key per domain, so it stays unbucketed.
    store.put(Rule("api.foo.com", 443, "backend.foo.com", "rule:apex"))
    assert conn.keys("r:{buckets:foo.com*") == [b"r:{buckets:foo.com}:api.foo.com:443"]


def test_bucketed_global_rule_matches_whichever_bucket_the_query_hashes_to(conn):
    store = RuleStore(conn, namespace="buckets", num_buckets=4)
    store.put(Rule(fqdn1="*", port=443, fqdn2="*", rule_id="rule:global"))

    for fqdn in BUCKET_PROBE_FQDNS:
        m = store.lookup(fqdn, 443, "backend.example.net", MatchMode.MULTI_LEVEL)
        assert m is not None, f"{fqdn} missed its global-tier replica"
        assert m.rule_id == "rule:global"
        assert m.fqdn1_pattern == "*" and m.fqdn2_pattern == "*"

    probed_candidates = [candidates(f, MatchMode.MULTI_LEVEL, num_buckets=4)[-1] for f in BUCKET_PROBE_FQDNS]
    assert {c.tag for c in probed_candidates} == {"__global__"}
    assert {c.bucket_index for c in probed_candidates} == {0, 1, 2, 3}


def test_bucketed_delete_clears_every_replica(conn):
    store = RuleStore(conn, namespace="buckets", num_buckets=4)
    store.put(Rule(fqdn1="*", port=443, fqdn2="*", rule_id="rule:global"))

    assert store.delete("*", 443, "*") is True
    assert conn.keys("r:buckets:*") == []
    assert store.delete("*", 443, "*") is False
    for fqdn in BUCKET_PROBE_FQDNS:
        assert store.lookup(fqdn, 443, "backend.example.net", MatchMode.MULTI_LEVEL) is None


def test_reopening_a_namespace_under_a_different_num_buckets_raises(conn):
    store = RuleStore(conn, namespace="buckets", num_buckets=4)
    store.put(Rule(fqdn1="*", port=443, fqdn2="*", rule_id="rule:global"))

    with pytest.raises(ValueError, match="num_buckets=8"):
        RuleStore(conn, namespace="buckets", num_buckets=8)

    same = RuleStore(conn, namespace="buckets", num_buckets=4)
    m = same.lookup("shop.omega.com", 443, "backend.example.net", MatchMode.MULTI_LEVEL)
    assert m is not None and m.rule_id == "rule:global"


def test_global_tier_bucket_index_varies_with_the_query_fqdn():
    probed = [candidates(f, MatchMode.MULTI_LEVEL, num_buckets=8)[-1] for f in BUCKET_PROBE_FQDNS]
    assert [c.tag for c in probed] == ["__global__"] * len(BUCKET_PROBE_FQDNS)
    assert [c.bucket_index for c in probed] == [0, 1, 2, 3, 4]


# ---------------------------------------------------------------------------
# Scaling: lookup cost must not grow with total registered-domain count
# ---------------------------------------------------------------------------


def test_lookup_cost_is_flat_as_total_domain_count_grows(conn):
    store = RuleStore(conn, namespace="scale")

    def populate(n_domains):
        domains = [f"tenant{i}.example.com" for i in range(n_domains)]
        store.put_many(
            Rule(f"api.{d}", 443, f"backend-api.{d}", "r-direct-api") for d in domains
        )
        return domains

    def measure(domains, samples=200):
        latencies = []
        for i in range(samples):
            d = domains[i % len(domains)]
            start = time.perf_counter()
            result = store.lookup(f"api.{d}", 443, f"backend-api.{d}", MatchMode.MULTI_LEVEL)
            latencies.append(time.perf_counter() - start)
            assert result is not None
        return statistics.median(latencies)

    conn.flushdb()
    small = populate(500)
    p50_small = measure(small)

    conn.flushdb()
    large = populate(5_000)
    p50_large = measure(large)

    assert p50_large < p50_small * 4, (
        f"lookup latency grew {p50_large / p50_small:.1f}x for a 10x domain-count increase "
        "-- hash-tag grouping may not be isolating lookups as intended"
    )
