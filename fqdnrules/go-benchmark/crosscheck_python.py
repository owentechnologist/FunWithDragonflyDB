"""Golden cross-check generator for the Go port.

Writes a fixed rule set through the Python RuleStore, runs a fixed query set
against it, and emits the rules, the queries, and the results as JSON. The Go
side replays all of it in golden_test.go and compares.

Run it, then run the Go half:

    python3 crosscheck_python.py /tmp/crosscheck.json
    FQDN_GOLDEN=/tmp/crosscheck.json go test -run TestGoldenCrossCheck -v

Set FQDN_NUM_BUCKETS to change the bucket count (default 4). Everything goes to
db 12 under namespace "xpy"; the Go side owns "xgo" on the same db, so neither
implementation can read the other's writes except where the test means it to.
"""
from __future__ import annotations

import json
import os
import sys
from pathlib import Path

sys.path.insert(0, "/Users/Shared/shared_github/scratch")

import redis

from fqdn_pattern import MatchMode, WildcardScope
from fqdn_router import Rule, RuleStore

DB = 12
NAMESPACE = "xpy"
NUM_BUCKETS = int(os.environ.get("FQDN_NUM_BUCKETS", "4"))

RULES = [
    ("api.foo.com", 443, "backend-api.foo.com", "r-exact", "B"),
    ("api.foo.com", 443, "*", "r-exact-any", "B"),
    ("*.corp.dragonfly.com", 443, "retailer.com", "r-corp-exact", "B"),
    ("*.corp.dragonfly.com", 443, "*.com", "r-corp-wild", "B"),
    ("*.dragonfly.com", 443, "retailer.com", "r-broad", "B"),
    ("*.dragonfly.com", 443, "*", "r-tls-strict", "S"),
    ("*.deep.example.org", 443, "*", "r-deep-only", "M"),
    ("*.com", 8443, "*.net", "r-tld", "B"),
    ("*", 443, "catch-all.example.com", "r-global", "B"),
    ("*", 80, "*", "r-global-any", "B"),
    ("host1.beta.co", 8443, "*.beta.co", "r-beta", "B"),
    ("host9.gamma.io", 8443, "backend9.gamma.io", "r-exact-multi-scope", "M"),
]

BUCKET_PROBES = [
    "shop.omega.com",
    "api.omega.net",
    "www.kappa.org",
    "portal.sigma.dev",
    "mail.lambda.io",
]

QUERIES = [
    ("api.foo.com", 443, "backend-api.foo.com", "single"),
    ("api.foo.com", 443, "backend-api.foo.com", "multi"),
    ("api.foo.com", 443, "whatever.xyz", "multi"),
    ("api.foo.com", 8080, "backend-api.foo.com", "multi"),
    ("console.corp.dragonfly.com", 443, "retailer.com", "single"),
    ("console.corp.dragonfly.com", 443, "shop.com", "single"),
    ("console.corp.dragonfly.com", 443, "retailer.com", "multi"),
    ("a.console.corp.dragonfly.com", 443, "retailer.com", "single"),
    ("a.console.corp.dragonfly.com", 443, "retailer.com", "multi"),
    ("host1.dragonfly.com", 443, "anything.net", "multi"),
    ("host1.dragonfly.com", 443, "anything.net", "single"),
    ("a.b.dragonfly.com", 443, "anything.net", "multi"),
    ("x.deep.example.org", 443, "zzz.net", "multi"),
    ("a.x.deep.example.org", 443, "zzz.net", "multi"),
    ("a.x.deep.example.org", 443, "zzz.net", "single"),
    ("shop.omega.com", 8443, "mail.gamma.net", "multi"),
    ("shop.omega.com", 8443, "mail.gamma.net", "single"),
    ("anything.invalid", 443, "catch-all.example.com", "multi"),
    ("localhost", 443, "catch-all.example.com", "multi"),
    ("host1.beta.co", 8443, "www.beta.co", "multi"),
    ("host1.beta.co", 8443, "host1.beta.co", "multi"),
    ("nomatch1.unknown.invalid", 8080, "nomatch1.invalid", "multi"),
    ("API.FOO.COM.", 443, "Backend-Api.Foo.Com", "multi"),
    ("host9.gamma.io", 8443, "backend9.gamma.io", "multi"),
] + [(probe, 80, "anything.at.all", "multi") for probe in BUCKET_PROBES]

MODES = {"single": MatchMode.SINGLE_LEVEL, "multi": MatchMode.MULTI_LEVEL}


def main() -> None:
    conn = redis.Redis(host="localhost", port=6379, db=DB)
    for key in conn.keys(f"r:{{{NAMESPACE}:*"):
        conn.delete(key)

    store = RuleStore(conn, namespace=NAMESPACE, num_buckets=NUM_BUCKETS)
    store.put_many(
        Rule(f1, port, f2, rid, WildcardScope(scope)) for f1, port, f2, rid, scope in RULES
    )

    results = []
    for fqdn1, port, fqdn2, mode in QUERIES:
        match = store.lookup(fqdn1, port, fqdn2, MODES[mode])
        results.append(
            None
            if match is None
            else {
                "rule_id": match.rule_id,
                "fqdn1_pattern": match.fqdn1_pattern,
                "port": match.port,
                "fqdn2_pattern": match.fqdn2_pattern,
                "rank1": match.specificity[0],
                "rank2": match.specificity[1],
            }
        )

    payload = {
        "db": DB,
        "num_buckets": NUM_BUCKETS,
        "rules": [
            {"fqdn1": f1, "port": port, "fqdn2": f2, "rule_id": rid, "scope": scope}
            for f1, port, f2, rid, scope in RULES
        ],
        "queries": [
            {"fqdn1": f1, "port": port, "fqdn2": f2, "mode": mode}
            for f1, port, f2, mode in QUERIES
        ],
        "expected": results,
    }
    out = Path(sys.argv[1])
    out.write_text(json.dumps(payload, indent=2))

    hits = sum(1 for r in results if r is not None)
    print(f"wrote {out} with {len(RULES)} rules, {len(QUERIES)} queries, {hits} hits, {len(QUERIES) - hits} misses")
    for q, r in zip(QUERIES, results):
        print(f"  {q[3]:6s} {q[0]:<30} {q[1]:5d} {q[2]:<25} -> {r['rule_id'] + ' ' + str((r['rank1'], r['rank2'])) if r else 'MISS'}")


if __name__ == "__main__":
    main()
