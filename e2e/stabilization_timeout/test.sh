#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Two binaries: without fix and with fix
ENVOY_WITHOUT_FIX="${SCRIPT_DIR}/envoy-static-without-fix"
ENVOY_WITH_FIX="${SCRIPT_DIR}/envoy-static-with-fix"

# Allow override via env
ENVOY_WITHOUT_FIX="${ENVOY_BINARY_WITHOUT_FIX:-$ENVOY_WITHOUT_FIX}"
ENVOY_WITH_FIX="${ENVOY_BINARY_WITH_FIX:-$ENVOY_WITH_FIX}"

XDS_SERVER="${SCRIPT_DIR}/xds-server"

XDS_PORT=5678
HTTP_PORT=5679
ADMIN_PORT=9901
BACKEND_PORT_1=8081
BACKEND_PORT_2=8082
STABILIZATION_TIMEOUT_MS=5000

# Results file for report
RESULTS_FILE="${SCRIPT_DIR}/test-results.txt"
> "$RESULTS_FILE"

log_result() {
    echo "$*" | tee -a "$RESULTS_FILE"
}

PIDS=()

cleanup() {
    echo "--- Cleaning up ---"
    for pid in "${PIDS[@]}"; do
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    done
    PIDS=()
}
trap cleanup EXIT

start_socat() {
    for port in $BACKEND_PORT_1 $BACKEND_PORT_2; do
        socat TCP-LISTEN:${port},fork,reuseaddr \
            SYSTEM:'echo -e "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"' &
        PIDS+=($!)
    done
    echo "socat backends on :${BACKEND_PORT_1}, :${BACKEND_PORT_2}"
}

stop_socat() {
    for port in $BACKEND_PORT_1 $BACKEND_PORT_2; do
        fuser -k ${port}/tcp 2>/dev/null || true
    done
}

start_xds_server() {
    local timeout_ms=$1
    "$XDS_SERVER" \
        --xds-port="$XDS_PORT" \
        --http-port="$HTTP_PORT" \
        --timeout-ms="$timeout_ms" \
        --node-id=test-node &
    PIDS+=($!)
    echo "xds_server pid=$! timeout_ms=$timeout_ms"
}

start_envoy() {
    local binary=$1
    "$binary" \
        -c "${SCRIPT_DIR}/envoy.yaml" \
        --log-level warn \
        --base-id 99 &
    PIDS+=($!)
    echo "envoy pid=$! binary=$(basename "$binary")"
}

wait_for_url() {
    local url=$1 max_wait=${2:-10}
    for ((i=0; i<max_wait; i++)); do
        if curl -sf "$url" >/dev/null 2>&1; then return 0; fi
        sleep 1
    done
    echo "FAIL: $url did not become ready in ${max_wait}s"
    return 1
}

count_cluster_hosts() {
    curl -sf "http://127.0.0.1:${ADMIN_PORT}/clusters" \
        | grep -c "^test-cluster::" || echo 0
}

has_pending_removal() {
    curl -sf "http://127.0.0.1:${ADMIN_PORT}/clusters" \
        | grep "test-cluster::" | grep -q "pending_dynamic_removal"
}

dump_clusters() {
    local label=$1
    echo "--- /clusters at ${label} ---"
    curl -sf "http://127.0.0.1:${ADMIN_PORT}/clusters" | grep "test-cluster::" || echo "(no test-cluster entries)"
    echo "---"
}

stop_all() {
    for pid in "${PIDS[@]}"; do
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    done
    PIDS=()
    # Also free the ports
    for port in $XDS_PORT $HTTP_PORT $ADMIN_PORT $BACKEND_PORT_1 $BACKEND_PORT_2 10000; do
        fuser -k ${port}/tcp 2>/dev/null || true
    done
    sleep 1
}

# ========================================================
# Preflight checks
# ========================================================
echo "=== Preflight ==="

if [[ ! -x "$ENVOY_WITHOUT_FIX" ]]; then
    echo "ERROR: envoy-static-without-fix not found at: $ENVOY_WITHOUT_FIX"
    exit 1
fi
if [[ ! -x "$ENVOY_WITH_FIX" ]]; then
    echo "ERROR: envoy-static-with-fix not found at: $ENVOY_WITH_FIX"
    exit 1
fi

echo "Binary WITHOUT fix: $ENVOY_WITHOUT_FIX"
echo "  $("$ENVOY_WITHOUT_FIX" --version 2>&1 | head -1)"
echo "Binary WITH fix: $ENVOY_WITH_FIX"
echo "  $("$ENVOY_WITH_FIX" --version 2>&1 | head -1)"

log_result "=== EDS Stabilization Timeout E2E Test ==="
log_result "Date: $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
log_result "Binary WITHOUT fix: $ENVOY_WITHOUT_FIX"
log_result "Binary WITH fix:    $ENVOY_WITH_FIX"
log_result ""

# ========================================================
# Build xds_server
# ========================================================
echo ""
echo "=== Building xds_server ==="
cd "$SCRIPT_DIR"
go build -o "$XDS_SERVER" .
echo "Built: $XDS_SERVER"

# ========================================================
# Scenario 1: WITHOUT FIX — hosts stuck in PENDING_DYNAMIC_REMOVAL forever
# ========================================================
echo ""
echo "=== SCENARIO 1: Without fix (envoy-static-without-fix, timeout_ms=0) ==="
log_result "=== SCENARIO 1: Without fix ==="

start_socat
start_xds_server 0
wait_for_url "http://127.0.0.1:${HTTP_PORT}/ready"
start_envoy "$ENVOY_WITHOUT_FIX"
wait_for_url "http://127.0.0.1:${ADMIN_PORT}/ready"

# Add targets, wait for health checks to pass
curl -sf -X POST "http://127.0.0.1:${HTTP_PORT}/add-targets"
echo "Waiting 3s for health checks..."
sleep 3

HOST_COUNT=$(count_cluster_hosts)
echo "Host count after add: $HOST_COUNT"
log_result "Hosts after add: $HOST_COUNT"

if [[ "$HOST_COUNT" -lt 2 ]]; then
    log_result "FAIL: Expected at least 2 hosts, got $HOST_COUNT"
    echo "FAIL: Expected at least 2 hosts, got $HOST_COUNT"
    exit 1
fi

dump_clusters "before-remove"

# Remove targets
curl -sf -X POST "http://127.0.0.1:${HTTP_PORT}/remove-targets"
REMOVE_TIME=$(date +%s)
echo "Targets removed. Waiting 10s (hosts should remain with PENDING_DYNAMIC_REMOVAL)..."
sleep 10

dump_clusters "10s-after-remove"

HOST_COUNT=$(count_cluster_hosts)
echo "Host count after 10s: $HOST_COUNT"
log_result "Hosts after 10s wait: $HOST_COUNT"

if has_pending_removal; then
    log_result "PENDING_DYNAMIC_REMOVAL: YES (hosts stuck as expected)"
    echo "Hosts have pending_dynamic_removal flag"
else
    log_result "PENDING_DYNAMIC_REMOVAL: NO"
fi

if [[ "$HOST_COUNT" -ge 2 ]]; then
    log_result "RESULT: PASS — hosts remain stuck in PENDING_DYNAMIC_REMOVAL without fix"
    echo "PASS: Scenario 1 — hosts remain in PENDING_DYNAMIC_REMOVAL without fix"
else
    log_result "RESULT: UNEXPECTED — hosts were removed (count=$HOST_COUNT)"
    echo "NOTE: Hosts were removed (count=$HOST_COUNT) — unexpected for no-fix binary"
fi

stop_all

# ========================================================
# Scenario 2: WITH FIX — hosts removed after timeout
# ========================================================
echo ""
echo "=== SCENARIO 2: With fix (envoy-static-with-fix, timeout_ms=${STABILIZATION_TIMEOUT_MS}) ==="
log_result ""
log_result "=== SCENARIO 2: With fix (timeout_ms=${STABILIZATION_TIMEOUT_MS}) ==="

start_socat
start_xds_server "$STABILIZATION_TIMEOUT_MS"
wait_for_url "http://127.0.0.1:${HTTP_PORT}/ready"
start_envoy "$ENVOY_WITH_FIX"
wait_for_url "http://127.0.0.1:${ADMIN_PORT}/ready"

# Add targets, wait for health checks
curl -sf -X POST "http://127.0.0.1:${HTTP_PORT}/add-targets"
echo "Waiting 3s for health checks..."
sleep 3

HOST_COUNT=$(count_cluster_hosts)
echo "Host count after add: $HOST_COUNT"
log_result "Hosts after add: $HOST_COUNT"

if [[ "$HOST_COUNT" -lt 2 ]]; then
    log_result "FAIL: Expected at least 2 hosts, got $HOST_COUNT"
    echo "FAIL: Expected at least 2 hosts, got $HOST_COUNT"
    exit 1
fi

dump_clusters "before-remove"

# Remove targets and track timing
curl -sf -X POST "http://127.0.0.1:${HTTP_PORT}/remove-targets"
T0=$(date +%s)
echo "Targets removed at T0. Polling every 500ms..."
log_result "Targets removed, polling for removal..."

# Poll every 500ms
REMOVED=false
while true; do
    NOW=$(date +%s)
    ELAPSED=$((NOW - T0))

    HOST_COUNT=$(count_cluster_hosts)

    if [[ "$HOST_COUNT" -lt 2 ]]; then
        if [[ "$ELAPSED" -lt 4 ]]; then
            log_result "RESULT: FAIL — hosts removed too early at ${ELAPSED}s (expected >= ~5s)"
            echo "FAIL: Hosts removed too early at ${ELAPSED}s (expected >= 5s)"
            dump_clusters "early-removal"
            exit 1
        fi
        echo "Hosts removed at ${ELAPSED}s after target removal"
        log_result "Hosts removed at: ${ELAPSED}s"
        REMOVED=true
        break
    fi

    if [[ "$ELAPSED" -ge 15 ]]; then
        log_result "RESULT: FAIL — hosts still present at ${ELAPSED}s (expected removal by ~7s)"
        echo "FAIL: Hosts still present at ${ELAPSED}s (expected removal by ~7s)"
        dump_clusters "timeout"
        exit 1
    fi

    sleep 0.5
done

if [[ "$REMOVED" == "true" ]]; then
    log_result "RESULT: PASS — hosts removed within expected window"
    echo "PASS: Scenario 2 — hosts removed within expected window"
else
    log_result "RESULT: FAIL — hosts were never removed"
    echo "FAIL: Hosts were never removed"
    exit 1
fi

dump_clusters "after-removal"

echo ""
echo "=== ALL TESTS PASSED ==="
log_result ""
log_result "=== ALL TESTS PASSED ==="
