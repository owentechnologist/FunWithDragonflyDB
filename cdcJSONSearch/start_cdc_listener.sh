#!/bin/bash

# Define infrastructure variables (with fallbacks)
REMOTE_HOST=${1:-"your-remote-redis-domain.com"}
REMOTE_PORT=${2:-"6379"}
USE_TLS=${3:-false}
APP_PORT=${4:-"3000"}

# Pull optional credentials from environment variables if present
REDIS_USER="${REDIS_USER:-""}"
REDIS_PASS="${REDIS_PASS:-""}"

rm -f key.pem cert.pem

yes | openssl req -x509 -newkey rsa:4096 -keyout key.pem -out cert.pem -nodes \
    -subj '/CN=localhost' -extensions SAN \
    -config <(cat /etc/ssl/openssl.cnf \
    <(printf "[SAN]\nsubjectAltName='DNS:localhost'"))

# Build arguments array dynamically based on whether credentials exist
ARGS=(
    --host "$REMOTE_HOST"
    --redis-port "$REMOTE_PORT"
    --port "$APP_PORT"
    --use-tls="$USE_TLS"
)

if [ -n "$REDIS_USER" ]; then
    ARGS+=(--username "$REDIS_USER")
fi

if [ -n "$REDIS_PASS" ]; then
    ARGS+=(--password "$REDIS_PASS")
fi

# Run the listener cleanly
#go run cdc_listener.go "${ARGS[@]}"
#go run cdc_multi_listener.go "${ARGS[@]}"
go run cdc_topic_guided_listener.go "${ARGS[@]}"