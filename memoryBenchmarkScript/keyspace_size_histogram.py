# keyspace_size_histogram.py 
import redis
import math
from collections import defaultdict
import ssl
from redis.cluster import RedisCluster

# 1. Setup an insecure TLS Context (Equivalent to tls-skip-verify)
ssl_context = ssl.create_default_context(purpose=ssl.Purpose.SERVER_AUTH)
ssl_context.check_hostname = False
ssl_context.verify_mode = ssl.CERT_NONE

print("Connecting to Dragonfly Cluster (TLS Verification Skipped)...")

# 2. Initialize the Cluster Client directly using host and port parameters
client = RedisCluster(
    host="whobrk4co.dragonflydb.cloud",  # Direct entrypoint host
    port=6385,                           # Direct entrypoint port
    ssl=True,
    username="default",                  
    password="4xyt2xy39yo1",                  # Replace with your actual password
    ssl_context=ssl_context,       
    decode_responses=True,
    skip_full_coverage_check=True
)

# Define our size buckets (in bytes)
BUCKETS = [1024, 10240, 102400, 1048576, 10485760, 104857600]
histogram = defaultdict(int)

print("Scanning Dragonfly database... (Safe for production)")

# Use scan_iter, which natively maps to Dragonfly's multi-threaded scan
for key in client.scan_iter(count=1000):
    try:
        size = client.memory_usage(key)
        if size is None:
            continue
        
        # Sort into the appropriate bucket
        placed = False
        for b in BUCKETS:
            if size <= b:
                histogram[b] += 1
                placed = True
                break
        if not placed:
            histogram['Large'] += 1
    except Exception:
        continue

print("\n=== KEY SIZE HISTOGRAM ===")
for b in BUCKETS:
    label = f"<= {b/1024:,.1f} KB" if b < 1048576 else f"<= {b/1048576:,.1f} MB"
    count = histogram[b]
    bar = "█" * min(count, 40)
    print(f"{label:<12} : {count:<6} {bar}")

large_count = histogram['Large']
large_bar = "█" * min(large_count, 40)
print(f"> 100.0 MB   : {large_count:<6} {large_bar}\n")
