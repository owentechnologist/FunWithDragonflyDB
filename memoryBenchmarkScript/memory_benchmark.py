#!/usr/bin/env python3
"""
Memory Benchmark: Dragonfly vs Redis/Valkey
============================================
Populates millions of objects across all supported data types and compares
memory utilization between Dragonfly and Redis/Valkey.

Requirements: pip install redis tabulate

Usage:
  python memory_benchmark.py # Use defaults (localhost:6379 vs localhost:6380)
  python memory_benchmark.py --dragonfly-port 6379 --redis-port 6380
  python memory_benchmark.py --types strings hashes sets # Only test specific types
  python memory_benchmark.py --scale 0.1 # 10% of default counts (quick test)
  python memory_benchmark.py --dragonfly-only # Skip Redis, just populate Dragonfly

  # Cloud / remote instances with TLS (no cert verification):
  python memory_benchmark.py --dragonfly-host my.cloud.host --dragonfly-tls --dragonfly-only

  # TLS with CA cert verification:
  python memory_benchmark.py --dragonfly-host my.cloud.host --dragonfly-tls \\
      --dragonfly-tls-ca-cert /path/to/ca.crt

  # Mutual TLS (mTLS) with client cert + key:
  python memory_benchmark.py --dragonfly-host my.cloud.host --dragonfly-tls \\
      --dragonfly-tls-ca-cert /path/to/ca.crt \\
      --dragonfly-tls-cert /path/to/client.crt \\
      --dragonfly-tls-key /path/to/client.key

  # Container-children sweep — track how memory scales as children grow:
  python memory_benchmark.py --dragonfly-only --container-children-count 200 --repeat 4
  # Dense upper-range sweep — extra fine-grained samples near the maximum:
  python memory_benchmark.py --dragonfly-only --container-children-count 1000 --repeat 10 --repeat 10
"""

import argparse
import copy
import json
import math
import random
import string
import sys
import time
from dataclasses import dataclass, field
from typing import Optional

try:
    import redis
except ImportError:
    print("ERROR: redis-py is required. Install with: pip install redis")
    sys.exit(1)

try:
    from tabulate import tabulate
except ImportError:
    tabulate = None # Graceful fallback

# =============================================================================
# CONFIGURATION — Tune these to your needs
# =============================================================================

@dataclass
class TypeConfig:
    """Configuration for a single data type benchmark."""
    enabled: bool = True
    num_keys: int = 1_000_000 # Number of keys to create
    # Type-specific parameters
    value_size: int = 64 # Bytes per value (strings, list/set elements)
    num_fields: int = 10 # Fields per hash / members per set / elements per list
    pipeline_batch: int = 5_000 # Commands per pipeline flush
    json_depth: str = "flat" # flat (~370B), medium (~500B), deep (~4.7KB)

# Default configurations per type
DEFAULT_CONFIGS = {
    "strings": TypeConfig(
        num_keys=10_000_000,
        value_size=64,
    ),
    "hashes": TypeConfig(
        num_keys=500_000,
        num_fields=10,
        value_size=32,
    ),
    "lists": TypeConfig(
        num_keys=500_000,
        num_fields=10, # Elements per list
        value_size=32,
    ),
    "sets": TypeConfig(
        num_keys=500_000,
        num_fields=15, # Members per set
        value_size=24,
    ),
    "sorted_sets": TypeConfig(
        num_keys=500_000,
        num_fields=15, # Members per zset
        value_size=24,
    ),
    "streams": TypeConfig(
        num_keys=500_000,
        num_fields=5, # Entries per stream
        value_size=32, # Value size per field in entry
    ),
    "hyperloglog": TypeConfig(
        num_keys=200_000,
        num_fields=100, # Elements to add per HLL
    ),
    "json": TypeConfig(
        enabled=False, # Disabled by default — needs RedisJSON module
        num_keys=500_000,
        num_fields=8, # Fields per JSON doc
        value_size=32,
        json_depth="flat",
    ),
    "json_flat": TypeConfig(
        enabled=False,
        num_keys=500_000,
        num_fields=8,
        value_size=32,
        json_depth="flat",
    ),
    "json_medium": TypeConfig(
        enabled=False,
        num_keys=500_000,
        num_fields=8,
        value_size=32,
        json_depth="medium",
    ),
    "json_deep": TypeConfig(
        enabled=False,
        num_keys=500_000,
        num_fields=8,
        value_size=32,
        json_depth="deep",
    ),
}

# =============================================================================
# HELPERS
# =============================================================================

def human_bytes(n: float) -> str:
    """Format bytes as human-readable string."""
    if n == 0:
        return "0 B"
    for unit in ["B", "KB", "MB", "GB", "TB"]:
        if abs(n) < 1024:
            return f"{n:.2f} {unit}"
        n /= 1024
    return f"{n:.2f} PB"

def random_value(size: int) -> str:
    """Generate a random string of given size."""
    return "".join(random.choices(string.ascii_letters + string.digits, k=size))

def get_memory(client: redis.Redis) -> int:
    """Get used_memory from INFO MEMORY."""
    info = client.info("memory")
    return info.get("used_memory", 0)

def check_module(client: redis.Redis, module_name: str) -> bool:
    """Check if a Redis module is loaded."""
    try:
        modules = client.module_list()
        return any(m.get(b"name", b"").decode().lower() == module_name.lower()
                    or m.get("name", "").lower() == module_name.lower()
                    for m in modules)
    except Exception:
        return False

def progress_bar(current: int, total: int, width: int = 40, prefix: str = "") -> str:
    pct = current / total if total else 1
    filled = int(width * pct)
    bar = "█" * filled + "░" * (width - filled)
    return f"\r {prefix}|{bar}| {pct*100:.0f}% ({current:,}/{total:,})"

# =============================================================================
# POPULATION FUNCTIONS — One per type
# =============================================================================

def populate_strings(client: redis.Redis, cfg: TypeConfig, key_prefix: str = "str", ttl: int = 0, verbose: bool = True) -> int:
    """
    Populate string keys. Uses DEBUG POPULATE for maximum speed when no TTL is set.
    Falls back to pipelined SET (with EX when TTL is set) otherwise.
    """
    if verbose:
        print(f" Populating {cfg.num_keys:,} strings (value_size={cfg.value_size})...")
    if ttl <= 0:
        try:
            # DEBUG POPULATE <count> <prefix> <value_size>
            client.execute_command("DEBUG", "POPULATE", cfg.num_keys, key_prefix, cfg.value_size)
            if verbose:
                print(f" ✓ Used DEBUG POPULATE")
            return cfg.num_keys
        except redis.ResponseError as e:
            if "unknown" in str(e).lower() or "debug" in str(e).lower():
                if verbose:
                    print(f" DEBUG POPULATE unavailable, falling back to pipelined SET...")
            else:
                raise

    # Pipelined SET — used when TTL is set or DEBUG POPULATE is unavailable
    pipe = client.pipeline(transaction=False)
    value = random_value(cfg.value_size)
    set_kwargs = {"ex": ttl} if ttl > 0 else {}
    for i in range(cfg.num_keys):
        pipe.set(f"{key_prefix}:{i}", value, **set_kwargs)
        if (i + 1) % cfg.pipeline_batch == 0:
            pipe.execute()
            if verbose:
                print(progress_bar(i + 1, cfg.num_keys, prefix="SET "), end="", flush=True)
    pipe.execute()
    if verbose:
        print()
    return cfg.num_keys

def populate_hashes(client: redis.Redis, cfg: TypeConfig, key_prefix: str = "hash", ttl: int = 0, verbose: bool = True) -> int:
    """Populate hash keys with N fields each."""
    total = cfg.num_keys
    if verbose:
        print(f" Populating {total:,} hashes ({cfg.num_fields} fields × {cfg.value_size}B values)...")

    pipe = client.pipeline(transaction=False)
    fields = {f"f{j}": random_value(cfg.value_size) for j in range(cfg.num_fields)}

    for i in range(total):
        key = f"{key_prefix}:{i}"
        pipe.hset(key, mapping=fields)
        if ttl > 0:
            pipe.expire(key, ttl)
        if (i + 1) % cfg.pipeline_batch == 0:
            pipe.execute()
            if verbose:
                print(progress_bar(i + 1, total, prefix="HSET "), end="", flush=True)
    pipe.execute()
    if verbose:
        print()
    return total

def populate_lists(client: redis.Redis, cfg: TypeConfig, key_prefix: str = "list", ttl: int = 0, verbose: bool = True) -> int:
    """Populate list keys with N elements each."""
    total = cfg.num_keys
    if verbose:
        print(f" Populating {total:,} lists ({cfg.num_fields} elements × {cfg.value_size}B)...")

    pipe = client.pipeline(transaction=False)
    elements = [random_value(cfg.value_size) for _ in range(cfg.num_fields)]

    for i in range(total):
        key = f"{key_prefix}:{i}"
        pipe.rpush(key, *elements)
        if ttl > 0:
            pipe.expire(key, ttl)
        if (i + 1) % cfg.pipeline_batch == 0:
            pipe.execute()
            if verbose:
                print(progress_bar(i + 1, total, prefix="RPUSH "), end="", flush=True)
    pipe.execute()
    if verbose:
        print()
    return total

def populate_sets(client: redis.Redis, cfg: TypeConfig, key_prefix: str = "set", ttl: int = 0, verbose: bool = True) -> int:
    """Populate set keys with N members each."""
    total = cfg.num_keys
    if verbose:
        print(f" Populating {total:,} sets ({cfg.num_fields} members × {cfg.value_size}B)...")

    pipe = client.pipeline(transaction=False)
    members = [random_value(cfg.value_size) for _ in range(cfg.num_fields)]

    for i in range(total):
        key = f"{key_prefix}:{i}"
        pipe.sadd(key, *members)
        if ttl > 0:
            pipe.expire(key, ttl)
        if (i + 1) % cfg.pipeline_batch == 0:
            pipe.execute()
            if verbose:
                print(progress_bar(i + 1, total, prefix="SADD "), end="", flush=True)
    pipe.execute()
    if verbose:
        print()
    return total

def populate_sorted_sets(client: redis.Redis, cfg: TypeConfig, key_prefix: str = "zset", ttl: int = 0, verbose: bool = True) -> int:
    """Populate sorted set keys with N scored members each."""
    total = cfg.num_keys
    if verbose:
        print(f" Populating {total:,} sorted sets ({cfg.num_fields} members × {cfg.value_size}B)...")

    pipe = client.pipeline(transaction=False)
    # Build {member: score} mapping
    members = {random_value(cfg.value_size): float(j) for j in range(cfg.num_fields)}

    for i in range(total):
        key = f"{key_prefix}:{i}"
        pipe.zadd(key, members)
        if ttl > 0:
            pipe.expire(key, ttl)
        if (i + 1) % cfg.pipeline_batch == 0:
            pipe.execute()
            if verbose:
                print(progress_bar(i + 1, total, prefix="ZADD "), end="", flush=True)
    pipe.execute()
    if verbose:
        print()
    return total

def populate_streams(client: redis.Redis, cfg: TypeConfig, key_prefix: str = "stream", ttl: int = 0, verbose: bool = True) -> int:
    """Populate stream keys with N entries each."""
    total = cfg.num_keys
    entries_per = cfg.num_fields
    if verbose:
        print(f" Populating {total:,} streams ({entries_per} entries, {cfg.value_size}B values)...")

    pipe = client.pipeline(transaction=False)
    entry_fields = {"field1": random_value(cfg.value_size), "field2": random_value(cfg.value_size)}

    for i in range(total):
        key = f"{key_prefix}:{i}"
        for _ in range(entries_per):
            pipe.xadd(key, entry_fields)
        if ttl > 0:
            pipe.expire(key, ttl)
        if (i + 1) % (cfg.pipeline_batch // max(entries_per, 1)) == 0:
            pipe.execute()
            if verbose:
                print(progress_bar(i + 1, total, prefix="XADD "), end="", flush=True)
    pipe.execute()
    if verbose:
        print()
    return total

def populate_hyperloglog(client: redis.Redis, cfg: TypeConfig, key_prefix: str = "hll", ttl: int = 0, verbose: bool = True) -> int:
    """Populate HyperLogLog keys with N/10 element additions each."""
    total = cfg.num_keys
    fields_per_key = round(cfg.num_fields / 10)
    if verbose:
        print(f" Populating {total:,} HyperLogLogs ({fields_per_key} elements each)...")

    pipe = client.pipeline(transaction=False)
    # Pre-generate element batches
    elements = [random_value(16) for _ in range(fields_per_key)]

    for i in range(total):
        key = f"{key_prefix}:{i}"
        pipe.pfadd(key, *elements)
        if ttl > 0:
            pipe.expire(key, ttl)
        if (i + 1) % cfg.pipeline_batch == 0:
            pipe.execute()
            if verbose:
                print(progress_bar(i + 1, total, prefix="PFADD "), end="", flush=True)
    pipe.execute()
    if verbose:
        print()
    return total

def _make_medium_doc(idx: int) -> dict:
    return {
        "times": [
            {"military": "0800", "civilian": "8 AM"},
            {"military": "1500", "civilian": "3 PM"},
            {"military": "2200", "civilian": "10 PM"},
        ],
        "responsible_parties": {
            "number_of_contacts": 2,
            "hosts": [
                {"phone": "715-876-5500", "name": f"Duncan Mills {idx}", "email": f"dmills{idx}@zew.org"},
                {"phone": "815-336-5500", "name": f"Xiria Andrus {idx}", "email": f"xiriaa{idx}@zew.org"},
            ],
        },
        "cost": 0,
        "name": f"Gorilla Feeding #{idx}",
        "days": ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"],
        "location": "Gorilla House South",
    }


def _make_deep_doc(idx: int) -> dict:
    num_cases = 3 + (idx % 5)  # 3–7 cases, varies by index
    injury_types = ["bruise", "injury", "wound", "fracture", "laceration"]
    incident_locations = [
        "Enclosure B-4", "Main Training Field", "Feeding Sector Alpha",
        "Recovery Zone C", "Observation Deck 2",
    ]
    treatment_types = [
        "Cold compression therapy",
        "Diagnostic X-Ray and Rest",
        "Antiseptic flush and bandaging",
        "Physical therapy and monitoring",
        "Surgical intervention and recovery",
    ]
    facility = {
        "role": "Marsupial Training Center",
        "species_served": ["Kangaroo", "Opossum", "Wombat", "Koala"],
        "current_gps_location": "117.14803,32.73259",
        "contact": {"name": "Front Desk", "phone": "832-677-5555"},
    }
    cases = []
    for c in range(num_cases):
        month = 3 + c
        day = 10 + c
        date_str = f"2026-{month:02d}-{day:02d}"
        treatment: dict = {
            "treatment_type": treatment_types[c % len(treatment_types)],
            "date_administered": date_str,
            "facility": facility,
        }
        if c % 2 == 0:  # attending vet on even cases
            treatment["attending_veterinarian"] = {
                "name": f"Dr. Vet-{idx}-{c}",
                "role": "veterinarian",
                "status": "hired",
                "species_speciality": "Kangaroo",
                "days_in_zoo": 1020,
                "search_set_rank": c + 1,
                "current_gps_location": "117.14803,32.73259",
                "description": f"An excellent vet with {12 + c * 2} years experience",
                "emergency_contact": {"name": f"Mrs Vet-{idx}-{c}", "phone": "722-766-2400"},
            }
        statuses = ["Monitoring", "Improving", "Fully Recovered"]
        recovery = [
            {
                "date": f"2026-{month:02d}-{day + 5 + r:02d}",
                "status": statuses[min(r, 2)],
                "notes": f"Recovery note {r + 1} for case {c + 1}.",
            }
            for r in range(c + 1)
        ]
        cases.append({
            "injury_type": injury_types[c % len(injury_types)],
            "date_occurred": date_str,
            "location_of_incident": incident_locations[c % len(incident_locations)],
            "treatments": [treatment],
            "recovery_progress_history": recovery,
        })
    return {
        "zoo_event": {
            "name": f"Gorilla Feeding #{idx}",
            "location": "Gorilla House South",
            "cost": 0,
            "days": ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"],
            "times": [
                {"military": "0800", "civilian": "8 AM"},
                {"military": "1500", "civilian": "3 PM"},
                {"military": "2200", "civilian": "10 PM"},
            ],
            "responsible_parties": {
                "number_of_contacts": 2,
                "hosts": [
                    {"name": f"Duncan Mills {idx}", "phone": "715-876-5500", "email": f"dmills{idx}@zew.org"},
                    {"name": f"Xiria Andrus {idx}", "phone": "815-336-5500", "email": f"xiriaa{idx}@zew.org"},
                ],
            },
        },
        "medical_records": {
            "total_incidents": num_cases,
            "cases": cases,
        },
    }


def populate_json(client: redis.Redis, cfg: TypeConfig, key_prefix: str = "", ttl: int = 0, verbose: bool = True) -> int:
    """Populate JSON keys (requires RedisJSON module)."""
    total = cfg.num_keys
    depth = cfg.json_depth
    key_prefix = key_prefix or f"json_{depth}"

    if depth == "medium":
        if verbose:
            print(f" Populating {total:,} JSON docs (medium ~500B, zoo schedule)...")
        docs = [json.dumps(_make_medium_doc(0))]
    elif depth == "deep":
        if verbose:
            print(f" Populating {total:,} JSON docs (deep ~4.7KB, zoo + medical records)...")
        # Pre-generate 5 docs covering case counts 3–7, then cycle
        docs = [json.dumps(_make_deep_doc(idx)) for idx in range(5)]
    else:  # flat
        if verbose:
            print(f" Populating {total:,} JSON docs (flat ~370B, {cfg.num_fields} fields × {cfg.value_size}B)...")
        docs = [json.dumps({f"field{j}": random_value(cfg.value_size) for j in range(cfg.num_fields)})]

    pipe = client.pipeline(transaction=False)
    pool_size = len(docs)

    for i in range(total):
        key = f"{key_prefix}:{i}"
        pipe.execute_command("JSON.SET", key, "$", docs[i % pool_size])
        if ttl > 0:
            pipe.expire(key, ttl)
        if (i + 1) % cfg.pipeline_batch == 0:
            pipe.execute()
            if verbose:
                print(progress_bar(i + 1, total, prefix="JSON.SET "), end="", flush=True)
    pipe.execute()
    if verbose:
        print()
    return total

# Map type names to their populate functions
POPULATORS = {
    "strings": populate_strings,
    "hashes": populate_hashes,
    "lists": populate_lists,
    "sets": populate_sets,
    "sorted_sets": populate_sorted_sets,
    "streams": populate_streams,
    "hyperloglog": populate_hyperloglog,
    "json": populate_json,
    "json_flat": populate_json,
    "json_medium": populate_json,
    "json_deep": populate_json,
}

# =============================================================================
# BENCHMARK ENGINE
# =============================================================================

@dataclass
class TypeResult:
    type_name: str
    num_keys: int
    total_elements: int # keys × fields/members
    memory_bytes: int
    time_seconds: float
    bytes_per_key: float = 0
    bytes_per_element: float = 0
    skipped: bool = False
    skip_reason: str = ""

    def __post_init__(self):
        if not self.skipped and self.num_keys > 0:
            self.bytes_per_key = self.memory_bytes / self.num_keys
            if self.total_elements > 0:
                self.bytes_per_element = self.memory_bytes / self.total_elements

def benchmark_instance(
    host: str,
    port: int,
    password: Optional[str],
    label: str,
    configs: dict[str, TypeConfig],
    type_order: list[str],
    ttl: int = 3600,
    tls: bool = False,
    tls_ca_cert: Optional[str] = None,
    tls_cert: Optional[str] = None,
    tls_key: Optional[str] = None,
    verbose: bool = True,
) -> dict[str, TypeResult]:
    """Run the full benchmark against one instance."""
    if verbose:
        tls_info = " [TLS]" if tls else ""
        print(f"\n{'='*70}")
        print(f" Benchmarking: {label} ({host}:{port}){tls_info}")
        print(f"{'='*70}")

    ssl_kwargs: dict = {}
    if tls:
        ssl_kwargs["ssl"] = True
        if tls_ca_cert:
            ssl_kwargs["ssl_ca_certs"] = tls_ca_cert
            ssl_kwargs["ssl_cert_reqs"] = "required"
        else:
            ssl_kwargs["ssl_cert_reqs"] = None
        if tls_cert:
            ssl_kwargs["ssl_certfile"] = tls_cert
        if tls_key:
            ssl_kwargs["ssl_keyfile"] = tls_key

    client = redis.Redis(host=host, port=port, password=password, decode_responses=False, **ssl_kwargs)

    # Connection check
    try:
        info = client.info("server")
        server_version = info.get("redis_version", info.get(b"redis_version", b"unknown"))
        if isinstance(server_version, bytes):
            server_version = server_version.decode()
        if verbose:
            print(f" Server version: {server_version}")
    except Exception as e:
        print(f" ✗ Cannot connect to {host}:{port}")
        print(f"   Error type : {type(e).__name__}")
        print(f"   Detail     : {e}")
        print(f"   Password   : {'provided (length=%d)' % len(password) if password else 'not set'}")
        if tls:
            print(f"   TLS        : enabled")
            print(f"   CA cert    : {tls_ca_cert or 'none (server cert not verified)'}")
            if tls_cert:
                print(f"   Client cert: {tls_cert}")
            if tls_key:
                print(f"   Client key : {tls_key}")
        else:
            print(f"   TLS        : disabled")
        print(f" Skipping {label}.")
        return {t: TypeResult(t, 0, 0, 0, 0, skipped=True, skip_reason="connection failed")
                for t in type_order}

    has_json = check_module(client, "ReJSON") or check_module(client, "rejson")

    results = {}

    # Single flush at the start so all type keys accumulate and remain visible at the pause
    client.flushall()
    time.sleep(0.5)
    prev_mem = get_memory(client)

    for type_name in type_order:
        cfg = configs.get(type_name)
        if not cfg or not cfg.enabled:
            results[type_name] = TypeResult(type_name, 0, 0, 0, 0, skipped=True, skip_reason="disabled")
            continue

        if type_name == "json" and not has_json:
            if verbose:
                print(f"\n [{type_name.upper()}] Skipped — RedisJSON module not loaded")
            results[type_name] = TypeResult(type_name, 0, 0, 0, 0, skipped=True, skip_reason="no module")
            continue

        if verbose:
            print(f"\n [{type_name.upper()}]")

        baseline = prev_mem

        t0 = time.time()
        num_keys = POPULATORS[type_name](client, cfg, ttl=ttl, verbose=verbose)
        elapsed = time.time() - t0

        time.sleep(1.0) # Let memory accounting settle
        final_mem = get_memory(client)
        delta = final_mem - baseline
        prev_mem = final_mem

        total_elements = num_keys * cfg.num_fields if type_name != "strings" else num_keys

        results[type_name] = TypeResult(
            type_name=type_name,
            num_keys=num_keys,
            total_elements=total_elements,
            memory_bytes=delta,
            time_seconds=elapsed,
        )

        if verbose:
            print(f" ✓ {num_keys:,} keys | Memory: {human_bytes(delta)} | "
                  f"{results[type_name].bytes_per_key:.1f} B/key | {elapsed:.1f}s")

    client.close()
    return results

# =============================================================================
# COMPARISON REPORT
# =============================================================================

def print_comparison(
    label_a: str, results_a: dict[str, TypeResult],
    label_b: str, results_b: dict[str, TypeResult],
    type_order: list[str],
):
    """Print a side-by-side comparison table."""
    print(f"\n{'='*90}")
    print(f" MEMORY COMPARISON: {label_a} vs {label_b}")
    print(f"{'='*90}\n")

    headers = [
        "Type",
        "Keys",
        f"{label_a}\nMemory",
        f"{label_a}\nB/key",
        f"{label_b}\nMemory",
        f"{label_b}\nB/key",
        "Savings",
        "Ratio",
    ]

    rows = []
    total_a = 0
    total_b = 0

    for t in type_order:
        ra = results_a.get(t)
        rb = results_b.get(t)

        if not ra or not rb or ra.skipped or rb.skipped:
            reason = ""
            if ra and ra.skipped:
                reason = ra.skip_reason
            elif rb and rb.skipped:
                reason = rb.skip_reason
            rows.append([t, "—", "—", "—", "—", "—", f"skipped ({reason})", "—"])
            continue

        total_a += ra.memory_bytes
        total_b += rb.memory_bytes

        a_neg = ra.memory_bytes < 0
        b_neg = rb.memory_bytes < 0

        if a_neg or b_neg:
            savings_str = "data store memory event occurred"
            ratio_str = "N/A"
        elif ra.memory_bytes > 0 and rb.memory_bytes > 0:
            savings_pct = (1 - ra.memory_bytes / rb.memory_bytes) * 100
            ratio = ra.memory_bytes / rb.memory_bytes
            savings_str = f"{savings_pct:+.1f}%"
            ratio_str = f"{ratio:.2f}x"
        else:
            savings_str = "N/A"
            ratio_str = "N/A"

        rows.append([
            t,
            f"{ra.num_keys:,}",
            "< 0 (artifact)" if a_neg else human_bytes(ra.memory_bytes),
            "—" if a_neg else f"{ra.bytes_per_key:.1f}",
            "< 0 (artifact)" if b_neg else human_bytes(rb.memory_bytes),
            "—" if b_neg else f"{rb.bytes_per_key:.1f}",
            savings_str,
            ratio_str,
        ])

    # Totals row
    if total_a > 0 and total_b > 0:
        total_savings = (1 - total_a / total_b) * 100
        total_ratio = total_a / total_b
        rows.append([
            "TOTAL", "—",
            human_bytes(total_a), "—",
            human_bytes(total_b), "—",
            f"{total_savings:+.1f}%",
            f"{total_ratio:.2f}x",
        ])

    if tabulate:
        print(tabulate(rows, headers=headers, tablefmt="rounded_grid", stralign="right"))
    else:
        # Fallback: simple aligned output
        print(f" {'Type':<14} {'Keys':>12} {label_a+' Mem':>14} {'B/key':>8} "
              f"{label_b+' Mem':>14} {'B/key':>8} {'Savings':>10} {'Ratio':>8}")
        print(" " + "-" * 94)
        for r in rows:
            print(f" {r[0]:<14} {r[1]:>12} {r[2]:>14} {r[3]:>8} "
                  f"{r[4]:>14} {r[5]:>8} {r[6]:>10} {r[7]:>8}")

    print(f"\n Savings = how much less {label_a} uses vs {label_b} [higher savings is better]")
    print(f" Ratio = {label_a} memory / {label_b} memory (lower percentage = {label_a} more efficient)\n")

def print_single_report(label: str, results: dict[str, TypeResult], type_order: list[str]):
    """Print results for a single instance."""
    print(f"\n{'='*70}")
    print(f" MEMORY REPORT: {label}")
    print(f"{'='*70}\n")

    headers = ["Type", "Keys", "Total Elements", "Memory", "B/key", "B/element", "Time"]
    rows = []
    total_mem = 0

    for t in type_order:
        r = results.get(t)
        if not r or r.skipped:
            continue
        total_mem += r.memory_bytes
        rows.append([
            t,
            f"{r.num_keys:,}",
            f"{r.total_elements:,}",
            human_bytes(r.memory_bytes),
            f"{r.bytes_per_key:.1f}",
            f"{r.bytes_per_element:.1f}" if r.total_elements > r.num_keys else "—",
            f"{r.time_seconds:.1f}s",
        ])

    rows.append(["TOTAL", "—", "—", human_bytes(total_mem), "—", "—", "—"])

    if tabulate:
        print(tabulate(rows, headers=headers, tablefmt="rounded_grid", stralign="right"))
    else:
        for r in rows:
            print(f" {r[0]:<14} {r[1]:>12} {r[2]:>16} {r[3]:>14} {r[4]:>8} {r[5]:>10} {r[6]:>8}")

# =============================================================================
# SWEEP ANALYSIS — Container-children growth impact
# =============================================================================

def compute_children_sequence(
    max_children: int,
    num_repeats: int,
    second_repeat: Optional[int] = None,
) -> list[int]:
    """
    Compute the ordered sequence of children counts for a sweep.

    Denominators = [max_children, num_repeats-1, num_repeats-2, ..., 2, 1]
    This gives a minimum case (max/max = 1 child) followed by num_repeats-1
    progressively larger counts up to max_children.

    num_repeats is capped at max_children to prevent the denominator list from
    including values that would only produce duplicates.  Deduplication is applied
    after rounding so that the actual step count may be less than num_repeats when
    the ratio max_children/N produces collisions.

    When second_repeat is given, an additional second_repeat fine-grained steps are
    inserted in the upper range (max_children/2, max_children) using fractional
    denominators 1+step, 1+2*step, ..., 1+(N-1)*step, 1+(N-1)*step+step/2 where
    step = 1/second_repeat.  This produces denser sampling near the maximum.

    Examples (single repeat):
      max=200, N=4 → denominators=[200,3,2,1] → [1, 67, 100, 200]
      max=200, N=2 → denominators=[200,1]     → [1, 200]
      max=5,   N=4 → denominators=[5,3,2,1]   → [1, 2, 3, 5]

    Example (with second_repeat=10, max=1000, num_repeats=10):
      Base:  [1, 100, 111, 125, 143, 167, 200, 250, 333, 500, 1000]
      Upper: adds ~909, 833, 769, 714, 667, 625, 588, 556, 526, 513
    """
    n = min(num_repeats, max_children)
    n = max(n, 1)
    denominators = [max_children] + list(range(n - 1, 0, -1))
    seen: set[int] = set()
    sequence: list[int] = []
    for d in denominators:
        count = max(1, round(max_children / d))
        if count not in seen:
            seen.add(count)
            sequence.append(count)

    if second_repeat is not None and second_repeat > 0:
        n2 = second_repeat
        step = 1.0 / n2
        upper_denoms = [1.0 + i * step for i in range(1, n2)]
        upper_denoms.append(1.0 + (n2 - 1) * step + step / 2)
        for d in upper_denoms:
            count = max(1, round(max_children / d))
            if count not in seen:
                seen.add(count)
                sequence.append(count)
        sequence.sort()

    return sequence


def print_sweep_report(
    sweep_results: dict,
    sequence: list[int],
    type_order: list[str],
    label_a: str,
    label_b: Optional[str] = None,
) -> None:
    """
    Print per-type memory progression tables from a container-children sweep.

    For container types (hashes, lists, sets, sorted_sets, streams) prints one
    table per server showing Memory, B/key, vs-prev-step, and vs-min-step columns.
    When two servers are present, also prints a savings-trend table per type.
    Non-container types (strings, hyperloglog, json variants) are omitted because
    their memory is unaffected by the children count.
    """
    CONTAINER_TYPES = {"hashes", "lists", "sets", "sorted_sets", "streams"}
    sweep_types = [t for t in type_order if t in CONTAINER_TYPES]

    all_labels = [label_a] if label_b is None else [label_a, label_b]

    print(f"\n{'='*70}")
    print(f" CONTAINER-CHILDREN SWEEP REPORT")
    print(f" Children sequence: {' → '.join(str(c) for c in sequence)}")
    print(f"{'='*70}")

    for type_name in sweep_types:
        print(f"\n  [{type_name.upper()}]")

        for server_label in all_labels:
            server_data = sweep_results.get(server_label, {})
            rows = []
            prev_mem: Optional[int] = None
            min_mem: Optional[int] = None

            # Determine key count from first successful result for the header
            num_keys = 0
            for c in sequence:
                r = server_data.get(c, {}).get(type_name)
                if r and not r.skipped:
                    num_keys = r.num_keys
                    break

            for children in sequence:
                result = server_data.get(children, {}).get(type_name)
                if result is None or result.skipped or result.memory_bytes <= 0:
                    rows.append([f"{children:,}", "—", "—", "—", "—"])
                    prev_mem = None
                    continue

                mem = result.memory_bytes
                bpk = result.bytes_per_key

                if min_mem is None:
                    min_mem = mem

                if prev_mem is not None and prev_mem > 0:
                    vs_prev = f"{mem / prev_mem:.2f}x"
                else:
                    vs_prev = "—"

                if min_mem is not None and min_mem > 0 and mem != min_mem:
                    vs_min = f"{mem / min_mem:.2f}x"
                else:
                    vs_min = "1.00x" if mem == min_mem else "—"

                rows.append([f"{children:,}", human_bytes(mem), f"{bpk:.1f}", vs_prev, vs_min])
                prev_mem = mem

            headers = ["Children", "Memory", "B/key", "vs prev", "vs min"]
            print(f"\n    {server_label}  ({num_keys:,} keys × N children)")
            if tabulate:
                print(tabulate(rows, headers=headers, tablefmt="rounded_grid", stralign="right"))
            else:
                print(f"    {'Children':>10}  {'Memory':>12}  {'B/key':>8}  {'vs prev':>8}  {'vs min':>8}")
                print("    " + "-" * 56)
                for r in rows:
                    print(f"    {r[0]:>10}  {r[1]:>12}  {r[2]:>8}  {r[3]:>8}  {r[4]:>8}")

        # Cross-server savings trend when two servers are present
        if label_b is not None:
            data_a = sweep_results.get(label_a, {})
            data_b = sweep_results.get(label_b, {})
            rows2 = []
            for children in sequence:
                ra = data_a.get(children, {}).get(type_name)
                rb = data_b.get(children, {}).get(type_name)
                if (ra is None or ra.skipped or ra.memory_bytes <= 0
                        or rb is None or rb.skipped or rb.memory_bytes <= 0):
                    rows2.append([f"{children:,}", "—", "—", "—", "—"])
                    continue
                savings = (1 - ra.memory_bytes / rb.memory_bytes) * 100
                ratio = ra.memory_bytes / rb.memory_bytes
                rows2.append([
                    f"{children:,}",
                    f"{ra.bytes_per_key:.1f}",
                    f"{rb.bytes_per_key:.1f}",
                    f"{savings:+.1f}%",
                    f"{ratio:.2f}x",
                ])
            headers2 = ["Children", f"{label_a} B/key", f"{label_b} B/key", "Savings", "Ratio"]
            print(f"\n    Savings trend  ({label_a} vs {label_b})")
            if tabulate:
                print(tabulate(rows2, headers=headers2, tablefmt="rounded_grid", stralign="right"))
            else:
                la_h = f"{label_a} B/key"
                lb_h = f"{label_b} B/key"
                print(f"    {'Children':>10}  {la_h:>16}  {lb_h:>16}  {'Savings':>8}  {'Ratio':>7}")
                print("    " + "-" * 66)
                for r in rows2:
                    print(f"    {r[0]:>10}  {r[1]:>16}  {r[2]:>16}  {r[3]:>8}  {r[4]:>7}")


def run_children_sweep(args) -> None:
    """
    Orchestrate a container-children sweep: run benchmark_instance for each
    children count in the computed sequence, then print comparison tables.

    Called from main() when --repeat is specified.  Suppresses per-type verbose
    output during iteration; prints a one-line status per (server, iteration) pair.
    """
    max_children = args.container_children_count  # validated before call in main()
    repeat1 = args.repeat[0]
    repeat2 = args.repeat[1] if len(args.repeat) > 1 else None
    sequence = compute_children_sequence(max_children, repeat1, repeat2)
    actual_n = len(sequence)

    # Build base configs without container_children_count applied so we can
    # override num_fields per iteration ourselves.
    saved_cc = args.container_children_count
    args.container_children_count = None
    base_configs = build_configs(args)
    args.container_children_count = saved_cc

    type_order = [t for t in DEFAULT_CONFIGS if base_configs[t].enabled]
    if not type_order:
        print("ERROR: No types enabled for sweep.")
        sys.exit(1)

    print(f"\n{'='*70}")
    print(f" CONTAINER-CHILDREN SWEEP")
    print(f" Children sequence: {' → '.join(str(c) for c in sequence)} ({actual_n} steps)")
    print(f" Types: {', '.join(type_order)}")
    print(f"{'='*70}\n")

    servers = []
    if not args.redis_only:
        servers.append((
            "Dragonfly",
            args.dragonfly_host, args.dragonfly_port, args.dragonfly_password,
            args.dragonfly_tls, args.dragonfly_tls_ca_cert,
            args.dragonfly_tls_cert, args.dragonfly_tls_key,
        ))
    if not args.dragonfly_only:
        servers.append((
            args.redis_label,
            args.redis_host, args.redis_port, args.redis_password,
            args.redis_tls, args.redis_tls_ca_cert,
            args.redis_tls_cert, args.redis_tls_key,
        ))

    # sweep_results[server_label][children_count][type_name] -> TypeResult
    sweep_results: dict[str, dict[int, dict[str, TypeResult]]] = {
        srv[0]: {} for srv in servers
    }

    total_iterations = len(servers) * actual_n
    iteration = 0

    for children in sequence:
        iter_configs = copy.deepcopy(base_configs)
        for tname in ("streams", "hashes", "lists", "sets", "sorted_sets"):
            if tname in iter_configs:
                iter_configs[tname].num_fields = children

        for (label, host, port, password, tls, tls_ca, tls_cert, tls_key) in servers:
            iteration += 1
            print(f" [{iteration:>{len(str(total_iterations))}}/"
                  f"{total_iterations}] {label}  children={children} ...",
                  end="", flush=True)
            t0 = time.time()

            results = benchmark_instance(
                host=host,
                port=port,
                password=password,
                label=label,
                configs=iter_configs,
                type_order=type_order,
                ttl=args.ttl,
                tls=tls,
                tls_ca_cert=tls_ca,
                tls_cert=tls_cert,
                tls_key=tls_key,
                verbose=False,
            )

            elapsed = time.time() - t0
            sweep_results[label][children] = results
            active = [t for t in type_order
                      if not results[t].skipped and results[t].memory_bytes > 0]
            total_mem = sum(results[t].memory_bytes for t in active)
            print(f"  {human_bytes(total_mem)}  ({elapsed:.0f}s)")

    label_a = servers[0][0]
    label_b = servers[1][0] if len(servers) == 2 else None
    print_sweep_report(sweep_results, sequence, type_order, label_a, label_b)

# =============================================================================
# MAIN
# =============================================================================

def parse_args():
    p = argparse.ArgumentParser(
        description="Memory benchmark: Dragonfly vs Redis/Valkey",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  %(prog)s # Default: localhost 6379 vs 6380
  %(prog)s --dragonfly-port 6379 --redis-port 6380
  %(prog)s --scale 0.1 # Quick run (10%% of default counts)
  %(prog)s --scale 5 # Large run (5x default counts)
  %(prog)s --types strings hashes sorted_sets # Only test specific types
  %(prog)s --dragonfly-only # Skip Redis entirely
  %(prog)s --string-keys 10000000 --hash-keys 2000000 # Override specific counts
  %(prog)s --dragonfly-only --container-children-count 200 --repeat 4  # Children sweep
  %(prog)s --dragonfly-only --container-children-count 1000 --repeat 10 --repeat 10  # Dense upper-range sweep
        """,
    )

    # Connection
    p.add_argument("--dragonfly-host", default="localhost")
    p.add_argument("--dragonfly-port", type=int, default=6379)
    p.add_argument("--dragonfly-password", default=None)
    p.add_argument("--dragonfly-tls", action="store_true",
                    help="Enable TLS for the Dragonfly connection")
    p.add_argument("--dragonfly-tls-ca-cert", default=None, metavar="PATH",
                    help="CA certificate file to verify the Dragonfly server certificate")
    p.add_argument("--dragonfly-tls-cert", default=None, metavar="PATH",
                    help="Client certificate file for Dragonfly mTLS")
    p.add_argument("--dragonfly-tls-key", default=None, metavar="PATH",
                    help="Client private key file for Dragonfly mTLS")
    p.add_argument("--redis-host", default="localhost")
    p.add_argument("--redis-port", type=int, default=6380)
    p.add_argument("--redis-password", default=None)
    p.add_argument("--redis-tls", action="store_true",
                    help="Enable TLS for the Redis/Valkey connection")
    p.add_argument("--redis-tls-ca-cert", default=None, metavar="PATH",
                    help="CA certificate file to verify the Redis/Valkey server certificate")
    p.add_argument("--redis-tls-cert", default=None, metavar="PATH",
                    help="Client certificate file for Redis/Valkey mTLS")
    p.add_argument("--redis-tls-key", default=None, metavar="PATH",
                    help="Client private key file for Redis/Valkey mTLS")
    p.add_argument("--redis-label", default="Redis/Valkey",
                    help="Label for the Redis/Valkey instance in reports")

    # Mode
    p.add_argument("--dragonfly-only", action="store_true",
                    help="Only benchmark Dragonfly (skip Redis/Valkey)")
    p.add_argument("--redis-only", action="store_true",
                    help="Only benchmark Redis/Valkey (skip Dragonfly)")

    # Type selection
    all_types = list(DEFAULT_CONFIGS.keys())
    p.add_argument("--types", nargs="+", choices=all_types, default=None,
                    help="Types to benchmark (default: all enabled)")
    p.add_argument("--enable-json", action="store_true",
                    help="Enable JSON type benchmark (requires RedisJSON)")
    p.add_argument("--json-depth", choices=["flat", "medium", "deep", "all"], default="flat",
                    help="JSON document complexity: flat (~370B), medium (~500B), deep (~4.7KB), "
                         "all=three separate rows (default: flat)")

    # Global scale
    p.add_argument("--scale", type=float, default=1.0,
                    help="Scale factor for all key counts (0.1 = 10%%, 2.0 = 200%%)")

    # Per-type key count overrides
    p.add_argument("--string-keys", type=int, default=None)
    p.add_argument("--hash-keys", type=int, default=None)
    p.add_argument("--list-keys", type=int, default=None)
    p.add_argument("--set-keys", type=int, default=None)
    p.add_argument("--zset-keys", type=int, default=None)
    p.add_argument("--stream-keys", type=int, default=None)
    p.add_argument("--hll-keys", type=int, default=None)
    p.add_argument("--json-keys", type=int, default=None)

    # Value tuning
    p.add_argument("--value-size", type=int, default=None,
                    help="Override value size for all types (bytes)")
    p.add_argument("--hash-fields", type=int, default=None,
                    help="Fields per hash")
    p.add_argument("--list-elements", type=int, default=None,
                    help="Elements per list")
    p.add_argument("--set-members", type=int, default=None,
                    help="Members per set")
    p.add_argument("--zset-members", type=int, default=None,
                    help="Members per sorted set")
    p.add_argument("--container-children-count", type=int, default=None,
                    help="Number of child/nested entries per key for streams, hashes, sets, and sorted sets")
    p.add_argument("--repeat", type=int, action="append", metavar="N",
                    help="Run a container-children sweep with N steps from 1 child up to "
                         "--container-children-count. Requires --container-children-count. "
                         "Specify --repeat a second time to add N fine-grained samples in "
                         "the upper range (max/2, max), giving denser coverage near the "
                         "maximum child count. If N exceeds --container-children-count it "
                         "is reduced to that value.")
    p.add_argument("--pipeline-batch", type=int, default=None,
                    help="Commands per pipeline flush (default: 5000)")
    p.add_argument("--ttl", type=int, default=3600,
                    help="TTL in seconds applied to all written keys (default: 3600). "
                         "Set to 0 to write keys with no expiry.")

    return p.parse_args()

def build_configs(args) -> dict[str, TypeConfig]:
    """Build type configs from defaults + CLI overrides."""
    configs = {k: copy.deepcopy(v) for k, v in DEFAULT_CONFIGS.items()}

    # Apply scale
    if args.scale != 1.0:
        for cfg in configs.values():
            cfg.num_keys = max(1000, int(cfg.num_keys * args.scale))

    # Apply container children count to all container types
    if args.container_children_count is not None:
        for type_name in ("streams", "hashes", "lists", "sets", "sorted_sets"):
            configs[type_name].num_fields = args.container_children_count

    # Enable JSON if requested
    if args.enable_json:
        configs["json"].enabled = True

    # Per-type key count overrides
    key_overrides = {
        "strings": args.string_keys,
        "hashes": args.hash_keys,
        "lists": args.list_keys,
        "sets": args.set_keys,
        "sorted_sets": args.zset_keys,
        "streams": args.stream_keys,
        "hyperloglog": args.hll_keys,
        "json": args.json_keys,
    }
    for type_name, override in key_overrides.items():
        if override is not None:
            configs[type_name].num_keys = override
            configs[type_name].enabled = True

    # HLL key count tracks strings unless --hll-keys was explicitly provided
    if args.hll_keys is None:
        configs["hyperloglog"].num_keys = configs["strings"].num_keys

    # Handle JSON depth modes
    if configs["json"].enabled:
        if args.json_depth == "all":
            json_num_keys = configs["json"].num_keys
            configs["json"].enabled = False
            for dt in ["json_flat", "json_medium", "json_deep"]:
                configs[dt].enabled = True
                configs[dt].num_keys = json_num_keys
        else:
            configs["json"].json_depth = args.json_depth

    # Global value size override
    if args.value_size is not None:
        for cfg in configs.values():
            cfg.value_size = args.value_size

    # Per-type field/member overrides
    if args.hash_fields is not None:
        configs["hashes"].num_fields = args.hash_fields
    if args.list_elements is not None:
        configs["lists"].num_fields = args.list_elements
    if args.set_members is not None:
        configs["sets"].num_fields = args.set_members
    if args.zset_members is not None:
        configs["sorted_sets"].num_fields = args.zset_members

    # Pipeline batch override
    if args.pipeline_batch is not None:
        for cfg in configs.values():
            cfg.pipeline_batch = args.pipeline_batch

    # Filter by --types if specified
    if args.types:
        for type_name in configs:
            if type_name not in args.types:
                configs[type_name].enabled = False
            else:
                configs[type_name].enabled = True

    return configs

def main():
    args = parse_args()

    # Sweep mode — branch early, no interactive pause, no normal report
    if args.repeat is not None:
        if args.container_children_count is None:
            print("ERROR: --repeat requires --container-children-count to be set.")
            sys.exit(1)
        run_children_sweep(args)
        print("\nDone.")
        return

    configs = build_configs(args)

    # Determine type execution order
    type_order = [t for t in DEFAULT_CONFIGS if configs[t].enabled]

    if not type_order:
        print("ERROR: No types enabled. Use --types or --enable-json.")
        sys.exit(1)

    # Print configuration summary
    print(f"\n{'='*70}")
    print(f" MEMORY BENCHMARK CONFIGURATION")
    print(f"{'='*70}")
    print(f" Scale factor: {args.scale}x")
    print(f" Key TTL: {args.ttl}s" if args.ttl > 0 else " Key TTL: none (keys persist until eviction/restart)")
    print(f" Types: {', '.join(type_order)}")
    for t in type_order:
        cfg = configs[t]
        detail = f"keys={cfg.num_keys:,}, value_size={cfg.value_size}"
        if t != "strings":
            detail += f", fields/members={cfg.num_fields}"
        if t.startswith("json"):
            detail += f", depth={cfg.json_depth}"
        print(f" {t:<14} {detail}")
    total_keys = sum(configs[t].num_keys for t in type_order)
    print(f" Total keys across all types: {total_keys:,}")
    print()

    # Warn when targeting remote hosts — loading millions of keys over WAN is slow
    _local = {"localhost", "127.0.0.1", "::1"}
    if not args.redis_only and args.dragonfly_host not in _local:
        print(f"WARNING: Dragonfly host '{args.dragonfly_host}' is not localhost. "
              "Loading large datasets over a remote connection can take significantly "
              "longer than a local benchmark — plan accordingly.\n")
    if not args.dragonfly_only and args.redis_host not in _local:
        print(f"WARNING: {args.redis_label} host '{args.redis_host}' is not localhost. "
              "Loading large datasets over a remote connection can take significantly "
              "longer than a local benchmark — plan accordingly.\n")

    dragonfly_results = None
    redis_results = None

    # Benchmark Dragonfly
    if not args.redis_only:
        dragonfly_results = benchmark_instance(
            host=args.dragonfly_host,
            port=args.dragonfly_port,
            password=args.dragonfly_password,
            label="Dragonfly",
            configs=configs,
            type_order=type_order,
            ttl=args.ttl,
            tls=args.dragonfly_tls,
            tls_ca_cert=args.dragonfly_tls_ca_cert,
            tls_cert=args.dragonfly_tls_cert,
            tls_key=args.dragonfly_tls_key,
        )

    # Benchmark Redis/Valkey
    if not args.dragonfly_only:
        redis_results = benchmark_instance(
            host=args.redis_host,
            port=args.redis_port,
            password=args.redis_password,
            label=args.redis_label,
            configs=configs,
            type_order=type_order,
            ttl=args.ttl,
            tls=args.redis_tls,
            tls_ca_cert=args.redis_tls_ca_cert,
            tls_cert=args.redis_tls_cert,
            tls_key=args.redis_tls_key,
        )

    # All datastores are now populated — pause for inspection before reporting/exit
    populated = []
    if dragonfly_results:
        populated.append(f"Dragonfly ({args.dragonfly_host}:{args.dragonfly_port})")
    if redis_results:
        populated.append(f"{args.redis_label} ({args.redis_host}:{args.redis_port})")
    print(f"\n{'='*70}")
    print(f" Keys are now written to all participating datastores:")
    for label in populated:
        print(f"   • {label}")
    print(f" You may now inspect and compare them with any Redis-compatible client.")
    print(f"{'='*70}")
    input(" Press Enter to print the final report and exit...\n")

    # Report
    if dragonfly_results and redis_results:
        print_comparison("Dragonfly", dragonfly_results, args.redis_label, redis_results, type_order)
    elif dragonfly_results:
        print_single_report("Dragonfly", dragonfly_results, type_order)
    elif redis_results:
        print_single_report(args.redis_label, redis_results, type_order)

    print("Done.")

if __name__ == "__main__":
    main()
