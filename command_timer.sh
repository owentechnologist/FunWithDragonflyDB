#!/bin/bash

# Check if at least a port and a command were provided
if [ $# -lt 2 ]; then
    echo "Usage: $0 <redis_port> <redis_command> [args...]"
    echo "Example: $0 6379 SET key value"
    exit 1
fi

# 1. Capture the port (the first argument)
PORT=$1

# 2. "Shift" the arguments to the left. 
# This discards the port ($1), making the old $2 become the new $1, $3 become $2, etc.
shift

# Capture the start time with nanosecond precision
START=$(date +%s.%N)

# 3. Execute redis-cli, passing ALL remaining arguments natively using "$@"
redis-cli -p "$PORT" "$@"

# Capture the end time with nanosecond precision
END=$(date +%s.%N)

# Calculate the difference using awk
DIFF=$(awk "BEGIN {printf \"%.6f\", $END - $START}")

# Display the time taken
echo ""
echo "Time taken: ${DIFF}s"
