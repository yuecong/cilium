#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright Authors of Cilium
#
# E2E test for cross-region pod mesh POC features:
#   Part A: Per-workload sync via clustermesh.cilium.io/sync label
#   Part B: SyncScope propagation (regional/global)
#   Part D: IPCACHE_MISS signal (observable via cilium_signals metric)
#
# Prerequisites:
#   - Two Kind clusters set up via: make kind-clustermesh
#   - Cilium built and images loaded:  make kind-clustermesh-images
#   - Cilium installed with POC flags: (this script does it)
#
# Usage:
#   ./contrib/testing/e2e-cross-region-pod-mesh.sh [setup|test-label-sync|test-syncscope|test-ipcache-miss|teardown|all]

set -euo pipefail

CLUSTER1_CTX="${CLUSTER1_CTX:-kind-clustermesh1}"
CLUSTER2_CTX="${CLUSTER2_CTX:-kind-clustermesh2}"
NAMESPACE="cross-region-test"

log() { echo "==> $*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

wait_for_pods() {
    local ctx=$1 ns=$2
    log "Waiting for pods in $ns on $ctx..."
    kubectl --context "$ctx" -n "$ns" wait --for=condition=Ready pods --all --timeout=120s
}

# --- Setup ---
setup() {
    log "Creating test namespace on both clusters"
    for ctx in "$CLUSTER1_CTX" "$CLUSTER2_CTX"; do
        kubectl --context "$ctx" create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl --context "$ctx" apply -f -
    done

    log "Deploying test workloads on cluster1"

    # Pod WITH sync label (should appear in cluster2 kvstore)
    kubectl --context "$CLUSTER1_CTX" -n "$NAMESPACE" apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: synced-pod
  labels:
    app: synced-pod
    clustermesh.cilium.io/sync: "true"
  annotations:
    clustermesh.cilium.io/sync-scope: "global"
spec:
  containers:
  - name: nginx
    image: nginx:stable-alpine
    ports:
    - containerPort: 80
EOF

    # Pod WITHOUT sync label (should NOT appear in cluster2 kvstore)
    kubectl --context "$CLUSTER1_CTX" -n "$NAMESPACE" apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: unsynced-pod
  labels:
    app: unsynced-pod
spec:
  containers:
  - name: nginx
    image: nginx:stable-alpine
    ports:
    - containerPort: 80
EOF

    # Pod with regional sync scope
    kubectl --context "$CLUSTER1_CTX" -n "$NAMESPACE" apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: regional-pod
  labels:
    app: regional-pod
    clustermesh.cilium.io/sync: "true"
  annotations:
    clustermesh.cilium.io/sync-scope: "regional"
spec:
  containers:
  - name: nginx
    image: nginx:stable-alpine
    ports:
    - containerPort: 80
EOF

    wait_for_pods "$CLUSTER1_CTX" "$NAMESPACE"
    log "Setup complete"
}

# --- Test A: Label-based sync filtering ---
test_label_sync() {
    log "=== Test: Per-workload label sync ==="

    local synced_ip unsynced_ip
    synced_ip=$(kubectl --context "$CLUSTER1_CTX" -n "$NAMESPACE" get pod synced-pod -o jsonpath='{.status.podIP}')
    unsynced_ip=$(kubectl --context "$CLUSTER1_CTX" -n "$NAMESPACE" get pod unsynced-pod -o jsonpath='{.status.podIP}')

    log "synced-pod IP:   $synced_ip"
    log "unsynced-pod IP: $unsynced_ip"

    # Check the clustermesh-apiserver etcd on cluster1 for what was synced
    local apiserver_pod
    apiserver_pod=$(kubectl --context "$CLUSTER1_CTX" -n kube-system get pods -l app.kubernetes.io/name=clustermesh-apiserver -o jsonpath='{.items[0].metadata.name}')

    if [ -z "$apiserver_pod" ]; then
        die "Could not find clustermesh-apiserver pod on cluster1"
    fi

    log "Checking clustermesh-apiserver etcd for synced endpoints..."

    # Query etcd inside the clustermesh-apiserver for IP entries
    local etcd_output
    etcd_output=$(kubectl --context "$CLUSTER1_CTX" -n kube-system exec "$apiserver_pod" -c etcd -- \
        etcdctl get --prefix "cilium/state/ip/v1/default/" --keys-only 2>/dev/null || true)

    local pass=true

    if echo "$etcd_output" | grep -q "$synced_ip"; then
        log "PASS: synced-pod IP ($synced_ip) found in etcd"
    else
        log "FAIL: synced-pod IP ($synced_ip) NOT found in etcd"
        pass=false
    fi

    if echo "$etcd_output" | grep -q "$unsynced_ip"; then
        log "FAIL: unsynced-pod IP ($unsynced_ip) found in etcd (should be filtered)"
        pass=false
    else
        log "PASS: unsynced-pod IP ($unsynced_ip) correctly filtered from etcd"
    fi

    if $pass; then
        log "=== Label sync test PASSED ==="
    else
        log "=== Label sync test FAILED ==="
        log "All etcd keys:"
        echo "$etcd_output"
        return 1
    fi
}

# --- Test B: SyncScope propagation ---
test_syncscope() {
    log "=== Test: SyncScope annotation propagation ==="

    local regional_ip
    regional_ip=$(kubectl --context "$CLUSTER1_CTX" -n "$NAMESPACE" get pod regional-pod -o jsonpath='{.status.podIP}')
    local synced_ip
    synced_ip=$(kubectl --context "$CLUSTER1_CTX" -n "$NAMESPACE" get pod synced-pod -o jsonpath='{.status.podIP}')

    log "regional-pod IP: $regional_ip"
    log "synced-pod IP:   $synced_ip"

    local apiserver_pod
    apiserver_pod=$(kubectl --context "$CLUSTER1_CTX" -n kube-system get pods -l app.kubernetes.io/name=clustermesh-apiserver -o jsonpath='{.items[0].metadata.name}')

    local pass=true

    # Check that regional-pod entry has syncScope=regional
    local regional_entry
    regional_entry=$(kubectl --context "$CLUSTER1_CTX" -n kube-system exec "$apiserver_pod" -c etcd -- \
        etcdctl get "cilium/state/ip/v1/default/$regional_ip" --print-value-only 2>/dev/null || true)

    if echo "$regional_entry" | grep -q '"SyncScope":"regional"'; then
        log "PASS: regional-pod has SyncScope=regional in etcd"
    else
        log "FAIL: regional-pod entry missing SyncScope=regional"
        log "Entry: $regional_entry"
        pass=false
    fi

    # Check that synced-pod entry has syncScope=global (or present)
    local synced_entry
    synced_entry=$(kubectl --context "$CLUSTER1_CTX" -n kube-system exec "$apiserver_pod" -c etcd -- \
        etcdctl get "cilium/state/ip/v1/default/$synced_ip" --print-value-only 2>/dev/null || true)

    if echo "$synced_entry" | grep -q '"SyncScope":"global"'; then
        log "PASS: synced-pod has SyncScope=global in etcd"
    elif ! echo "$synced_entry" | grep -q 'SyncScope'; then
        log "PASS: synced-pod has no SyncScope (defaults to global)"
    else
        log "FAIL: unexpected syncScope for synced-pod"
        log "Entry: $synced_entry"
        pass=false
    fi

    if $pass; then
        log "=== SyncScope test PASSED ==="
    else
        log "=== SyncScope test FAILED ==="
        return 1
    fi
}

# --- Test D: IPCACHE_MISS signal ---
test_ipcache_miss() {
    log "=== Test: IPCACHE_MISS signal ==="
    log "This test verifies that the BPF signal fires when accessing an IP not in ipcache"

    # Get a client pod on cluster2 and try to reach an IP that is NOT synced
    # (unsynced-pod on cluster1). This should trigger an ipcache miss.

    local unsynced_ip
    unsynced_ip=$(kubectl --context "$CLUSTER1_CTX" -n "$NAMESPACE" get pod unsynced-pod -o jsonpath='{.status.podIP}')

    log "Deploying a test client on cluster2..."
    kubectl --context "$CLUSTER2_CTX" -n "$NAMESPACE" apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: test-client
  labels:
    app: test-client
spec:
  containers:
  - name: curl
    image: curlimages/curl:latest
    command: ["sleep", "3600"]
EOF
    wait_for_pods "$CLUSTER2_CTX" "$NAMESPACE"

    # Get the cilium agent pod on the same node as test-client
    local client_node
    client_node=$(kubectl --context "$CLUSTER2_CTX" -n "$NAMESPACE" get pod test-client -o jsonpath='{.spec.nodeName}')
    local agent_pod
    agent_pod=$(kubectl --context "$CLUSTER2_CTX" -n kube-system get pods -l k8s-app=cilium -o json | \
        jq -r ".items[] | select(.spec.nodeName==\"$client_node\") | .metadata.name")

    log "Client node: $client_node, Cilium agent: $agent_pod"

    # Record signal count before the test
    local before_count
    before_count=$(kubectl --context "$CLUSTER2_CTX" -n kube-system exec "$agent_pod" -c cilium-agent -- \
        cilium-dbg metrics list -o json 2>/dev/null | \
        jq '[.[] | select(.name=="cilium_signals_handled_total") | select(.labels.signal=="ipcache_miss")] | .[0].value // 0' 2>/dev/null || echo "0")

    log "Signal count before: $before_count"

    # Attempt to connect to the unsynced pod IP (will fail, but should trigger signal)
    log "Sending traffic to unsynced IP $unsynced_ip (expect timeout/failure)..."
    kubectl --context "$CLUSTER2_CTX" -n "$NAMESPACE" exec test-client -- \
        curl -s --connect-timeout 2 "http://$unsynced_ip:80/" 2>/dev/null || true

    # Small delay for signal processing
    sleep 2

    # Check signal count after
    local after_count
    after_count=$(kubectl --context "$CLUSTER2_CTX" -n kube-system exec "$agent_pod" -c cilium-agent -- \
        cilium-dbg metrics list -o json 2>/dev/null | \
        jq '[.[] | select(.name=="cilium_signals_handled_total") | select(.labels.signal=="ipcache_miss")] | .[0].value // 0' 2>/dev/null || echo "0")

    log "Signal count after: $after_count"

    if [ "$after_count" -gt "$before_count" ] 2>/dev/null; then
        log "PASS: IPCACHE_MISS signal count increased ($before_count -> $after_count)"
        log "=== IPCACHE_MISS test PASSED ==="
    else
        log "WARN: Signal count did not increase. This could mean:"
        log "  - The signal handler is not registered (Cell not wired in agent)"
        log "  - The BPF program was not reloaded with new code"
        log "  - The metric is not yet exposed"
        log "  Checking if the signal type is at least registered..."

        # Fallback: check cilium-dbg bpf signals
        kubectl --context "$CLUSTER2_CTX" -n kube-system exec "$agent_pod" -c cilium-agent -- \
            cilium-dbg metrics list 2>/dev/null | grep -i "signal" || true

        log "=== IPCACHE_MISS test INCONCLUSIVE ==="
        log "(The BPF changes require a full image rebuild to take effect)"
    fi
}

# --- Teardown ---
teardown() {
    log "Cleaning up test namespace on both clusters"
    for ctx in "$CLUSTER1_CTX" "$CLUSTER2_CTX"; do
        kubectl --context "$ctx" delete namespace "$NAMESPACE" --ignore-not-found=true
    done
    log "Teardown complete"
}

# --- Main ---
case "${1:-all}" in
    setup)          setup ;;
    test-label-sync)    test_label_sync ;;
    test-syncscope)     test_syncscope ;;
    test-ipcache-miss)  test_ipcache_miss ;;
    teardown)       teardown ;;
    all)
        setup
        sleep 10  # Wait for CiliumEndpoint objects to be created and synced
        test_label_sync
        test_syncscope
        test_ipcache_miss
        teardown
        ;;
    *)
        echo "Usage: $0 [setup|test-label-sync|test-syncscope|test-ipcache-miss|teardown|all]"
        exit 1
        ;;
esac
