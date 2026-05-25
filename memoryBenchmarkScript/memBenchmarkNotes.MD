# ALERT: THIS IS A DESTRUCTIVE SCRIPT THAT FLUSHES ALL DATA BEFORE IT BEGINS!!!

**Types covered:** (note all types and nested quantities [how many elemnts to write within SETS, HASHES, LISTS, etc] are configurable)
**Strings** (5M) — uses DEBUG POPULATE for speed, falls back to pipelined SET
**Hashes** (1M) — 10 fields × 32B values
**Lists** (500K) — 20 elements × 32B
**Sets** (500K) — 15 members × 24B
**Sorted Sets** (500K) — 15 scored members × 24B
**Streams** (100K) — 5 entries, 2 fields × 32B each
**HyperLogLog** (200K) — 100 element additions each
**JSON** (500K) — disabled by default, enable with --enable-json (needs RedisJSON on Redis side; built-in on Dragonfly)

**~8.3M keys / ~50M+ elements at defaults.**

# Python Environment Setup 
- **A: Create a virtual environment:**

```
python3 -m venv bmenv
```

- **B. Activate it:  [This step is repeated anytime you want this venv back]**

```
source bmenv/bin/activate
```

On windows you would do:

```
bmenv\Scripts\activate
```
If no permission in Windows
 The Fix (Temporary, Safe, Local):
In PowerShell as Administrator, run:
```

Set-ExecutionPolicy -Scope CurrentUser -ExecutionPolicy RemoteSigned
```
Then confirm with Y when prompted.


- **C. Install the libraries: [only necesary to do this one time per environment]**

```
pip3 install -r requirements.txt
```


**Quick start:**bash
###pip install redis tabulate

# Full run (Dragonfly on 6379, Redis on 6380)
python3 memory_benchmark.py

# Quick test at 1% scale
```
python memory_benchmark.py --scale 0.01
```

# Quick test at 10% scale
```
python memory_benchmark.py --scale 0.1
```

# Only specific types
```
python memory_benchmark.py --types strings hashes sorted_sets
```

# Override key counts
```
python memory_benchmark.py --string-keys 10000000 --hash-fields 20
```

# Compare against Redis-Stack
```
python memory_benchmark.py --redis-label Stack --enable-json --redis-port 10000
```

# Sample run: (summary section)

```BASH
python3 memory_benchmark.py --enable-json --json-depth all --redis-port 10000 --redis-label Stack --ttl 300 --container-children-count 200 --dragonfly-host myhost.com --dragonfly-password yummypassword --dragonfly-tls --dragonfly-port 6385 --scale .0025
```

<details><summary>Sample Output:</summary>
<p>

```

==========================================================================================
 MEMORY COMPARISON: Stack vs Dragonfly
==========================================================================================

╭─────────────┬────────┬───────────┬─────────┬─────────────┬─────────────┬───────────┬─────────╮
│        Type │   Keys │     Stack │   Stack │   Dragonfly │   Dragonfly │   Savings │   Ratio │
│             │        │    Memory │   B/key │      Memory │       B/key │           │         │
├─────────────┼────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│     strings │ 25,000 │   4.51 MB │   189.2 │     2.12 MB │        88.9 │    +53.0% │   2.13x │
├─────────────┼────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│      hashes │  1,250 │   9.55 MB │  8008.4 │    18.54 MB │     15552.3 │    -94.2% │   0.51x │
├─────────────┼────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│       lists │  1,250 │   8.23 MB │  6905.1 │     9.12 MB │      7646.9 │    -10.7% │   0.90x │
├─────────────┼────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│        sets │  1,250 │  13.86 MB │ 11624.6 │    10.75 MB │      9016.2 │    +22.4% │   1.29x │
├─────────────┼────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│ sorted_sets │  1,250 │  29.42 MB │ 24682.7 │    16.92 MB │     14191.9 │    +42.5% │   1.74x │
├─────────────┼────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│     streams │  1,250 │  20.10 MB │ 16858.1 │    19.97 MB │     16750.3 │     +0.6% │   1.01x │
├─────────────┼────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│ hyperloglog │ 25,000 │  12.27 MB │   514.5 │     9.95 MB │       417.3 │    +18.9% │   1.23x │
├─────────────┼────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│   json_flat │  1,250 │ 452.48 KB │   370.7 │     1.10 MB │       922.4 │   -148.8% │   0.40x │
├─────────────┼────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│ json_medium │  1,250 │   1.55 MB │  1297.0 │     1.77 MB │      1481.4 │    -14.2% │   0.88x │
├─────────────┼────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│   json_deep │  1,250 │  10.75 MB │  9015.7 │    13.75 MB │     11530.8 │    -27.9% │   0.78x │
├─────────────┼────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│       TOTAL │      — │ 110.67 MB │       — │   103.97 MB │           — │     +6.1% │   1.06x │
╰─────────────┴────────┴───────────┴─────────┴─────────────┴─────────────┴───────────┴─────────╯

 Savings = how much less Dragonfly uses vs Stack
 Ratio = Stack memory / Dragonfly memory (higher = Dragonfly more efficient)

Done.
```


</details>


## And a run where no JSON keys are written

```BASH
 python3 memory_benchmark.py --redis-port 10000 --redis-label Stack --ttl 300 --dragonfly-host mycloud.com --dragonfly-password yummypassword --dragonfly-tls --dragonfly-port 6385 --scale .25
```

<details><summary>Sample Output 2:</summary>
<p>

```
==========================================================================================
 MEMORY COMPARISON: Stack vs Dragonfly
==========================================================================================

╭─────────────┬───────────┬───────────┬─────────┬─────────────┬─────────────┬───────────┬─────────╮
│        Type │      Keys │     Stack │   Stack │   Dragonfly │   Dragonfly │   Savings │   Ratio │
│             │           │    Memory │   B/key │      Memory │       B/key │           │         │
├─────────────┼───────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│     strings │ 2,500,000 │ 480.55 MB │   201.6 │   280.12 MB │       117.5 │    +41.7% │   1.72x │
├─────────────┼───────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│      hashes │   125,000 │  58.16 MB │   487.9 │    55.31 MB │       464.0 │     +4.9% │   1.05x │
├─────────────┼───────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│       lists │   125,000 │  54.36 MB │   456.0 │    47.68 MB │       400.0 │    +12.3% │   1.14x │
├─────────────┼───────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│        sets │   125,000 │  44.10 MB │   369.9 │    87.74 MB │       736.0 │    -99.0% │   0.50x │
├─────────────┼───────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│ sorted_sets │   125,000 │  63.88 MB │   535.9 │    55.31 MB │       464.0 │    +13.4% │   1.15x │
├─────────────┼───────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│     streams │   125,000 │ 537.90 MB │  4512.2 │   511.64 MB │      4291.9 │     +4.9% │   1.05x │
├─────────────┼───────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│ hyperloglog │ 2,500,000 │   1.22 GB │   522.6 │  -208.49 MB │       -87.4 │       N/A │     N/A │
├─────────────┼───────────┼───────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┤
│       TOTAL │         — │   2.43 GB │       — │   829.31 MB │           — │    +66.6% │   3.00x │
╰─────────────┴───────────┴───────────┴─────────┴─────────────┴─────────────┴───────────┴─────────╯

 Savings = how much less Dragonfly uses vs Stack
 Ratio = Stack memory / Dragonfly memory (higher = Dragonfly more efficient)

Done.
```

</details>

Everything is configurable — key counts, value sizes, field counts, pipeline batch size, and you can --scale 5 for a large run or --scale 0.001 for a quick sanity check. Keys are written with TTL of 1 hour by default (example above uses 5 min). The output reports per-type memory delta, bytes/key, savings %, and ratio.