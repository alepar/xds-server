# EDS Host Removal Stabilization Timeout — E2E Test Report

## Test Date and Environment

- **Date**: 2026-03-04 03:26 UTC
- **Host**: Linux 6.12.63+deb13-cloud-amd64
- **Test location**: `/home/debian/AleCode/gt/xds_server/mayor/rig/e2e/stabilization_timeout/`

## Binary Versions

| Binary | Version | Commit | Build |
|--------|---------|--------|-------|
| envoy-static-with-fix | 1.38.0-dev | 6f6efef31f (Clean) | RELEASE/BoringSSL |
| envoy-static-without-fix | 1.38.0-dev | 6f6efef31f (Modified) | RELEASE/BoringSSL |

Note: The without-fix binary was built from the same commit with the fix source files
(`eds.cc`, `eds.h`, `eds_test.cc`) reverted to the parent commit state. The "Modified"
flag reflects the dirty working tree at build time.

## Test Setup

- **xDS server**: Custom Go control plane pushing CDS+EDS snapshots via gRPC
- **Backend**: Python HTTP servers on ports 8081, 8082 (respond 200 to all requests)
- **Health checking**: HTTP health checks every 1s on `/healthz`
- **Cluster**: EDS-type `test-cluster` with active health checking
- **Stabilization timeout**: Configured via cluster metadata `envoy.eds.host_removal_stabilization_timeout_ms`

## Scenario 1: Without Fix (timeout_ms=0)

**Purpose**: Verify that without the stabilization timeout, removed EDS hosts remain
stuck in `PENDING_DYNAMIC_REMOVAL` indefinitely.

| Step | Result |
|------|--------|
| Hosts after EDS add | 2 (healthy) |
| Hosts 10s after EDS remove | 2 (pending_dynamic_removal) |
| PENDING_DYNAMIC_REMOVAL flag | Present on both hosts |

**Result: PASS**

Hosts correctly remain in `PENDING_DYNAMIC_REMOVAL` state indefinitely after being
removed from the EDS response. This confirms the baseline behavior: once health-checked
hosts enter `pending_dynamic_removal`, there is no mechanism to clean them up without
the stabilization timeout fix.

## Scenario 2: With Fix (timeout_ms=5000)

**Purpose**: Verify that with the stabilization timeout configured at 5000ms, removed
EDS hosts are cleaned up after the timeout period (expected removal at ~5-7s).

| Step | Result |
|------|--------|
| Hosts after EDS add | 2 (healthy) |
| Hosts after EDS remove | 2 (pending_dynamic_removal) |
| Expected removal window | 5-7s after removal |
| Actual removal | NOT REMOVED after 15s |

**Result: FAIL**

Hosts remained in `PENDING_DYNAMIC_REMOVAL` state for the full 15s observation period,
identical to the without-fix behavior. The stabilization timeout metadata
(`envoy.eds.host_removal_stabilization_timeout_ms=5000`) was set on the cluster
via CDS but did not trigger host removal.

## Analysis

The fix's stabilization timeout mechanism is not triggering host removal in this
e2e test scenario. Possible root causes:

1. **Metadata not being read**: The cluster metadata `envoy.eds.host_removal_stabilization_timeout_ms`
   may not be parsed from the CDS-delivered cluster config. The implementation may
   expect the metadata in a different location or format.

2. **Timer not firing**: The stabilization timeout timer may not be started when hosts
   enter `pending_dynamic_removal` state via EDS endpoint removal.

3. **Health check interaction**: The active health checker may be preventing host
   removal even after the stabilization timeout expires. The existing Envoy behavior
   keeps health-checked hosts in `pending_dynamic_removal` to allow draining.

4. **EDS update path**: The stabilization timeout may only apply to specific EDS
   update paths (e.g., locality changes) rather than full endpoint removal.

## Conclusion

- **Scenario 1 (baseline)**: PASS — confirms hosts get stuck in `PENDING_DYNAMIC_REMOVAL`
- **Scenario 2 (with fix)**: FAIL — stabilization timeout does not trigger host cleanup

The fix needs investigation to determine why the timeout-based host removal is not
functioning in this e2e scenario. The unit tests for the fix pass (verified separately),
suggesting the issue may be in how the metadata is delivered via CDS or how the timeout
interacts with active health checking.

## Raw Test Results

```
=== EDS Stabilization Timeout E2E Test ===
Date: 2026-03-04 03:26:12 UTC
Binary WITHOUT fix: envoy-static-without-fix
Binary WITH fix:    envoy-static-with-fix

=== SCENARIO 1: Without fix ===
Hosts after add: 2
Hosts after 10s wait: 2
PENDING_DYNAMIC_REMOVAL: YES (hosts stuck as expected)
RESULT: PASS — hosts remain stuck in PENDING_DYNAMIC_REMOVAL without fix

=== SCENARIO 2: With fix (timeout_ms=5000) ===
Hosts after add: 2
Targets removed, polling for removal...
RESULT: FAIL — hosts still present at 15s (expected removal by ~7s)
```
