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
python3 memory_benchmark.py --enable-json --json-depth all --redis-port 10000 --redis-label Stack --ttl 300 --container-children-count 550 --scale .01 --dragonfly-host myhost.com --dragonfly-password yummypassword --dragonfly-tls --dragonfly-port 6385 
```

<details><summary>Sample Output:</summary>
<p>

```
======================================================================
 Benchmarking: Dragonfly (localhost:6379)
======================================================================
 Server version: 7.4.0


======================================================================
 Benchmarking: stack (localhost:10000)
======================================================================
 Server version: 7.4.7

==========================================================================================
 MEMORY COMPARISON: Dragonfly vs Stack
==========================================================================================

╭─────────────┬─────────┬─────────────┬─────────────┬───────────┬─────────┬───────────┬─────────╮
│        Type │    Keys │   Dragonfly │   Dragonfly │     Stack │   Stack │   Savings │   Ratio │
│             │         │      Memory │       B/key │    Memory │   B/key │           │         │
├─────────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│     strings │ 100,000 │     6.13 MB │        64.3 │  18.03 MB │   189.0 │    +66.0% │   0.34x │
├─────────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│      hashes │   5,000 │   209.83 MB │     44003.9 │ 270.69 MB │ 56768.4 │    +22.5% │   0.78x │
├─────────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│       lists │   5,000 │    92.23 MB │     19342.5 │  90.52 MB │ 18984.1 │     -1.9% │   1.02x │
├─────────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│        sets │   5,000 │   127.41 MB │     26720.6 │ 158.12 MB │ 33160.1 │    +19.4% │   0.81x │
├─────────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│ sorted_sets │   5,000 │   193.75 MB │     40632.3 │ 329.46 MB │ 69092.8 │    +41.2% │   0.59x │
├─────────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│     streams │   5,000 │   217.48 MB │     45609.3 │ 217.78 MB │ 45672.3 │     +0.1% │   1.00x │
├─────────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│ hyperloglog │ 100,000 │    10.45 MB │       109.6 │  21.07 MB │   220.9 │    +50.4% │   0.50x │
├─────────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│   json_flat │   5,000 │     6.09 MB │      1276.5 │   1.82 MB │   381.2 │   -234.9% │   3.35x │
├─────────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│ json_medium │   5,000 │     8.26 MB │      1731.9 │   6.18 MB │  1296.3 │    -33.6% │   1.34x │
├─────────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│   json_deep │   5,000 │    55.71 MB │     11682.8 │  42.96 MB │  9009.8 │    -29.7% │   1.30x │
├─────────────┼─────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│       TOTAL │       — │   927.34 MB │           — │   1.13 GB │       — │    +19.8% │   0.80x │
╰─────────────┴─────────┴─────────────┴─────────────┴───────────┴─────────┴───────────┴─────────╯

 Savings = how much less Dragonfly uses vs Stack [higher savings is better]
 Ratio = Dragonfly memory / Stack memory (lower percentage = Dragonfly more efficient)

Done.
```


</details>


## And a larger run where no JSON keys are written (note with a large number of keys it is important to avoid expirations from occurring in the middle of the load so set the TTL accordingly or risk seeing memory anomalies)

```BASH
python3 memory_benchmark.py --redis-label stack --redis-port 10000 --scale 0.1 --container-children-count 550 --ttl 1800

```

<details><summary>Sample Output 2:</summary>
<p>

```

======================================================================
 Benchmarking: Dragonfly (localhost:6379)
======================================================================
 Server version: 7.4.0


======================================================================
 Benchmarking: stack (localhost:10000)
======================================================================
 Server version: 7.4.7

==========================================================================================
 MEMORY COMPARISON: Dragonfly vs stack
==========================================================================================

╭─────────────┬───────────┬─────────────┬─────────────┬───────────┬─────────┬───────────┬─────────╮
│        Type │      Keys │   Dragonfly │   Dragonfly │     stack │   stack │   Savings │   Ratio │
│             │           │      Memory │       B/key │    Memory │   B/key │           │         │
├─────────────┼───────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│     strings │ 1,000,000 │   124.77 MB │       130.8 │ 176.25 MB │   184.8 │    +29.2% │   0.71x │
├─────────────┼───────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│      hashes │    50,000 │     2.05 GB │     43984.7 │   2.67 GB │ 57440.3 │    +23.4% │   0.77x │
├─────────────┼───────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│       lists │    50,000 │   913.24 MB │     19152.0 │ 905.29 MB │ 18985.3 │     -0.9% │   1.01x │
├─────────────┼───────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│        sets │    50,000 │     1.23 GB │     26384.0 │   1.54 GB │ 33016.9 │    +20.1% │   0.80x │
├─────────────┼───────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│ sorted_sets │    50,000 │     1.88 GB │     40336.0 │   3.22 GB │ 69092.4 │    +41.6% │   0.58x │
├─────────────┼───────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│     streams │    50,000 │     2.12 GB │     45589.1 │   2.12 GB │ 45547.8 │     -0.1% │   1.00x │
├─────────────┼───────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│ hyperloglog │ 1,000,000 │   129.05 MB │       135.3 │ 190.76 MB │   200.0 │    +32.3% │   0.68x │
├─────────────┼───────────┼─────────────┼─────────────┼───────────┼─────────┼───────────┼─────────┤
│       TOTAL │         — │     8.42 GB │           — │  10.79 GB │       — │    +22.0% │   0.78x │
╰─────────────┴───────────┴─────────────┴─────────────┴───────────┴─────────┴───────────┴─────────╯

 Savings = how much less Dragonfly uses vs stack [higher savings is better]
 Ratio = Dragonfly memory / stack memory (lower percentage = Dragonfly more efficient)

Done.

```

</details>

Everything is configurable — key counts, value sizes, field counts, pipeline batch size, and you can --scale 5 for a large run or --scale 0.001 for a quick sanity check. Keys are written with TTL of 1 hour by default (example above specify several min). The output reports per-type memory delta, bytes/key, savings %, and ratio.