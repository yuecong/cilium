# Cross-Region Pod Mesh POC — E2E Test Guide

## What This Tests

This POC implements workload-controlled sync and ad-hoc sync for Cilium Cluster Mesh, as
proposed in [CFP: Cluster Mesh Scaling to 100+ Clusters](https://github.com/cilium/design-cfps/pull/89).
All changes in this branch are in the context of that CFP.

- **Part A — Source-side label filtering**: Only CiliumEndpoints with
  `clustermesh.cilium.io/sync: "true"` are synced to remote clusters
- **Part B — SyncScope propagation**: `clustermesh.cilium.io/sync-scope` annotation
  (`regional`/`global`) propagates to kvstore via `IPIdentityPair`
- **Part C — Consumer-side regional filtering**: KVStoreMesh reflector skips endpoints
  with `sync-scope=regional` from clusters in a different region
- **Part D — Ad-hoc BPF signal**: BPF emits `SIGNAL_IPCACHE_MISS` when a destination
  resolves to WORLD identity (no /32 entry), enabling on-demand resolution

## Prerequisites

- Docker (20.10+)
- `kind` and `helm` binaries
- Built POC images (see below)

## Build Images

From the `poc-cross-region-v1.17` branch:

```bash
DOCKER_BUILDKIT=1 docker build -f images/cilium/Dockerfile -t localhost/cilium/cilium-dev:poc-v1.17 --target release . &
DOCKER_BUILDKIT=1 docker build -f images/operator/Dockerfile -t localhost/cilium/operator-generic:poc-v1.17 --build-arg OPERATOR_VARIANT=operator-generic --target release . &
DOCKER_BUILDKIT=1 docker build -f images/clustermesh-apiserver/Dockerfile -t localhost/cilium/clustermesh-apiserver:poc-v1.17 --target release . &
wait
```

## Create Clusters

```bash
kind create cluster --name clustermesh1 --config - <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  podSubnet: "10.1.0.0/16"
  serviceSubnet: "172.20.1.0/24"
  disableDefaultCNI: true
nodes:
- role: control-plane
EOF

kind create cluster --name clustermesh2 --config - <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  podSubnet: "10.2.0.0/16"
  serviceSubnet: "172.20.2.0/24"
  disableDefaultCNI: true
nodes:
- role: control-plane
EOF

# Load images
for c in clustermesh1 clustermesh2; do
  kind load docker-image \
    localhost/cilium/cilium-dev:poc-v1.17 \
    localhost/cilium/operator-generic:poc-v1.17 \
    localhost/cilium/clustermesh-apiserver:poc-v1.17 \
    -n "$c"
done
```

## Install Cilium

Get the Kind node Docker IPs (needed for TLS SANs):

```bash
C1_IP=$(docker inspect clustermesh1-control-plane -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
C2_IP=$(docker inspect clustermesh2-control-plane -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
```

Install on cluster1:

```bash
helm install cilium ./install/kubernetes/cilium \
  --namespace kube-system --kube-context kind-clustermesh1 \
  --set cluster.name=clustermesh1 --set cluster.id=1 \
  --set image.override=localhost/cilium/cilium-dev:poc-v1.17 --set image.pullPolicy=Never \
  --set operator.image.override=localhost/cilium/operator-generic:poc-v1.17 --set operator.image.pullPolicy=Never \
  --set ipam.mode=kubernetes \
  --set clustermesh.useAPIServer=true --set clustermesh.config.enabled=true \
  --set clustermesh.apiserver.image.override=localhost/cilium/clustermesh-apiserver:poc-v1.17 \
  --set clustermesh.apiserver.image.pullPolicy=Never \
  --set clustermesh.apiserver.kvstoremesh.enabled=false \
  --set-string 'clustermesh.apiserver.extraEnv[0].name=CILIUM_ENABLE_SYNC_LABEL_FILTER' \
  --set-string 'clustermesh.apiserver.extraEnv[0].value=true' \
  --set "clustermesh.apiserver.tls.server.extraIpAddresses[0]=${C1_IP}"
```

Copy the shared CA and install on cluster2:

```bash
kubectl --context kind-clustermesh1 get secret -n kube-system cilium-ca -o yaml | \
  kubectl --context kind-clustermesh2 apply -f -

helm install cilium ./install/kubernetes/cilium \
  --namespace kube-system --kube-context kind-clustermesh2 \
  --set cluster.name=clustermesh2 --set cluster.id=2 \
  --set image.override=localhost/cilium/cilium-dev:poc-v1.17 --set image.pullPolicy=Never \
  --set operator.image.override=localhost/cilium/operator-generic:poc-v1.17 --set operator.image.pullPolicy=Never \
  --set ipam.mode=kubernetes \
  --set clustermesh.useAPIServer=true --set clustermesh.config.enabled=true \
  --set clustermesh.apiserver.image.override=localhost/cilium/clustermesh-apiserver:poc-v1.17 \
  --set clustermesh.apiserver.image.pullPolicy=Never \
  --set clustermesh.apiserver.kvstoremesh.enabled=false \
  --set-string 'clustermesh.apiserver.extraEnv[0].name=CILIUM_ENABLE_SYNC_LABEL_FILTER' \
  --set-string 'clustermesh.apiserver.extraEnv[0].value=true' \
  --set "clustermesh.apiserver.tls.server.extraIpAddresses[0]=${C2_IP}"
```

Wait for all pods:

```bash
for ctx in kind-clustermesh1 kind-clustermesh2; do
  kubectl --context "$ctx" -n kube-system wait --for=condition=Ready pods -l k8s-app=cilium --timeout=180s
  kubectl --context "$ctx" -n kube-system wait --for=condition=Ready pods -l app.kubernetes.io/name=clustermesh-apiserver --timeout=120s
done
```

## Connect Clusters

Exchange the remote client certs and etcd configs:

```bash
C1_PORT=$(kubectl --context kind-clustermesh1 -n kube-system get svc clustermesh-apiserver -o jsonpath='{.spec.ports[0].nodePort}')
C2_PORT=$(kubectl --context kind-clustermesh2 -n kube-system get svc clustermesh-apiserver -o jsonpath='{.spec.ports[0].nodePort}')

# Get remote certs
C1_CA=$(kubectl --context kind-clustermesh1 -n kube-system get secret clustermesh-apiserver-remote-cert -o jsonpath='{.data.ca\.crt}')
C1_CERT=$(kubectl --context kind-clustermesh1 -n kube-system get secret clustermesh-apiserver-remote-cert -o jsonpath='{.data.tls\.crt}')
C1_KEY=$(kubectl --context kind-clustermesh1 -n kube-system get secret clustermesh-apiserver-remote-cert -o jsonpath='{.data.tls\.key}')
C2_CA=$(kubectl --context kind-clustermesh2 -n kube-system get secret clustermesh-apiserver-remote-cert -o jsonpath='{.data.ca\.crt}')
C2_CERT=$(kubectl --context kind-clustermesh2 -n kube-system get secret clustermesh-apiserver-remote-cert -o jsonpath='{.data.tls\.crt}')
C2_KEY=$(kubectl --context kind-clustermesh2 -n kube-system get secret clustermesh-apiserver-remote-cert -o jsonpath='{.data.tls\.key}')

# Create etcd client configs
C1_CFG=$(printf 'endpoints:\n- https://%s:%s\ntrusted-ca-file: /var/lib/cilium/clustermesh/clustermesh1-ca.crt\ncert-file: /var/lib/cilium/clustermesh/clustermesh1.crt\nkey-file: /var/lib/cilium/clustermesh/clustermesh1.key\n' "$C1_IP" "$C1_PORT" | base64 -w0)
C2_CFG=$(printf 'endpoints:\n- https://%s:%s\ntrusted-ca-file: /var/lib/cilium/clustermesh/clustermesh2-ca.crt\ncert-file: /var/lib/cilium/clustermesh/clustermesh2.crt\nkey-file: /var/lib/cilium/clustermesh/clustermesh2.key\n' "$C2_IP" "$C2_PORT" | base64 -w0)

# Patch secrets
kubectl --context kind-clustermesh2 -n kube-system patch secret cilium-clustermesh \
  -p "{\"data\":{\"clustermesh1\":\"$C1_CFG\",\"clustermesh1-ca.crt\":\"$C1_CA\",\"clustermesh1.crt\":\"$C1_CERT\",\"clustermesh1.key\":\"$C1_KEY\"}}"
kubectl --context kind-clustermesh1 -n kube-system patch secret cilium-clustermesh \
  -p "{\"data\":{\"clustermesh2\":\"$C2_CFG\",\"clustermesh2-ca.crt\":\"$C2_CA\",\"clustermesh2.crt\":\"$C2_CERT\",\"clustermesh2.key\":\"$C2_KEY\"}}"

# Restart agents to pick up the connection config
kubectl --context kind-clustermesh1 -n kube-system delete pod -l k8s-app=cilium
kubectl --context kind-clustermesh2 -n kube-system delete pod -l k8s-app=cilium
```

Verify connection (wait ~60s):

```bash
for ctx in kind-clustermesh1 kind-clustermesh2; do
  AGENT=$(kubectl --context "$ctx" -n kube-system get pods -l k8s-app=cilium -o jsonpath='{.items[0].metadata.name}')
  kubectl --context "$ctx" -n kube-system exec "$AGENT" -c cilium-agent -- cilium-dbg status | grep ClusterMesh
done
# Expected: "ClusterMesh: 1/1 remote clusters ready" on both
```

## Run the E2E Test

Create test pods on cluster1 — two with the sync label, one without:

```bash
kubectl --context kind-clustermesh1 create namespace test-poc

# Pod WITH sync label + regional scope
kubectl --context kind-clustermesh1 -n test-poc apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: synced-regional
  labels:
    app: synced-regional
    clustermesh.cilium.io/sync: "true"
  annotations:
    clustermesh.cilium.io/sync-scope: "regional"
spec:
  containers:
  - name: nginx
    image: nginx:stable-alpine
EOF

# Pod WITH sync label + global scope
kubectl --context kind-clustermesh1 -n test-poc apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: synced-global
  labels:
    app: synced-global
    clustermesh.cilium.io/sync: "true"
  annotations:
    clustermesh.cilium.io/sync-scope: "global"
spec:
  containers:
  - name: nginx
    image: nginx:stable-alpine
EOF

# Pod WITHOUT sync label — should NOT appear on cluster2
kubectl --context kind-clustermesh1 -n test-poc apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: unsynced
  labels:
    app: unsynced
spec:
  containers:
  - name: nginx
    image: nginx:stable-alpine
EOF

kubectl --context kind-clustermesh1 -n test-poc wait --for=condition=Ready pods --all --timeout=60s
```

## Verify Results

Wait ~15s for sync, then check cluster2's BPF ipcache:

```bash
SR_IP=$(kubectl --context kind-clustermesh1 -n test-poc get pod synced-regional -o jsonpath='{.status.podIP}')
SG_IP=$(kubectl --context kind-clustermesh1 -n test-poc get pod synced-global -o jsonpath='{.status.podIP}')
UN_IP=$(kubectl --context kind-clustermesh1 -n test-poc get pod unsynced -o jsonpath='{.status.podIP}')

C2_AGENT=$(kubectl --context kind-clustermesh2 -n kube-system get pods -l k8s-app=cilium -o jsonpath='{.items[0].metadata.name}')
IPCACHE=$(kubectl --context kind-clustermesh2 -n kube-system exec "$C2_AGENT" -c cilium-agent -- cilium-dbg bpf ipcache list)

echo "$IPCACHE" | grep -q "$SR_IP" && echo "PASS: synced-regional visible" || echo "FAIL"
echo "$IPCACHE" | grep -q "$SG_IP" && echo "PASS: synced-global visible"   || echo "FAIL"
echo "$IPCACHE" | grep -q "$UN_IP" && echo "FAIL: unsynced visible!"       || echo "PASS: unsynced filtered"
```

### Expected Output

```
PASS: synced-regional visible
PASS: synced-global visible
PASS: unsynced filtered
```

Also confirm the apiserver logged the filtering:

```bash
APISERVER=$(kubectl --context kind-clustermesh1 -n kube-system get pods -l app.kubernetes.io/name=clustermesh-apiserver -o jsonpath='{.items[0].metadata.name}')
kubectl --context kind-clustermesh1 -n kube-system logs "$APISERVER" -c apiserver | grep -E "Sync-label|Upserting endpoint"
```

Expected:
```
Sync-label filtering enabled: only CiliumEndpoints with clustermesh.cilium.io/sync=true will be synced
Upserting endpoint in etcd  endpoint=test-poc/synced-regional ipAddr=10.1.x.x
Upserting endpoint in etcd  endpoint=test-poc/synced-global   ipAddr=10.1.x.x
```

Note: `unsynced` should NOT appear in the upsert logs.

## Confirmed Results

The following was confirmed on a two-cluster Kind setup (kernel `5.4.0-1154-aws-fips`,
Docker `20.10.22`, Kind `v0.25.0`).

### 1. Label filtering works end-to-end

Only the 2 labeled endpoints were upserted to etcd and propagated to cluster2.
The unlabeled pod was filtered out and does not appear in cluster2's ipcache.

**Cluster1 apiserver logs** — only labeled pods are upserted, `unsynced` is absent:

```
time="2026-03-21T18:30:40Z" level=info msg="Sync-label filtering enabled: only CiliumEndpoints with clustermesh.cilium.io/sync=true will be synced"
time="2026-03-21T18:51:34Z" level=info msg="Upserting endpoint in etcd" endpoint=test-poc/synced-regional ipAddr=10.1.0.215
time="2026-03-21T18:51:34Z" level=info msg="Upserting endpoint in etcd" endpoint=test-poc/synced-global   ipAddr=10.1.0.10
```

**Cluster2 BPF ipcache** — only the two synced /32 entries from cluster1 appear, with
correct tunnel endpoint. The unsynced pod (`10.1.0.164`) is absent:

```
$ cilium-dbg bpf ipcache list | grep "10\.1\.0\."
10.1.0.10/32     identity=118772 encryptkey=0 tunnelendpoint=172.18.0.2
10.1.0.215/32    identity=97304  encryptkey=0 tunnelendpoint=172.18.0.2
10.1.0.233/32    identity=4      encryptkey=0 tunnelendpoint=172.18.0.2   # cilium-agent health
10.1.0.86/32     identity=6      encryptkey=0 tunnelendpoint=172.18.0.2   # kube-dns
```

Note: `10.1.0.164` (unsynced pod) does NOT appear — the label filter prevented it from
being written to etcd, so cluster2 never sees it.

### 2. SyncScope propagation

The `clustermesh.cilium.io/sync-scope` annotation on the Pod is carried through to the
`IPIdentityPair.SyncScope` field in the etcd JSON value. This was verified in the
single-cluster unit/integration tests (txtar):

```
# cilium/state/ip/v1/default/10.1.0.1
{
  "IP": "10.1.0.1",
  "Mask": null,
  "HostIP": "172.18.0.3",
  "ID": 200000,
  ...
  "SyncScope": "regional"
}
```

Endpoints without the annotation produce no `SyncScope` field (omitempty), which consumers
treat as global.

### 3. Cross-cluster ipcache populated

Cluster2's BPF ipcache has /32 entries for the synced pods with
`tunnelendpoint=172.18.0.2` (cluster1's node IP). This confirms the full data path is set
up: cluster1 apiserver writes to etcd, cluster2 agent watches etcd and populates its local
BPF ipcache map. A pod on cluster2 sending traffic to `10.1.0.10` would use the VXLAN
tunnel to reach cluster1's node at `172.18.0.2`.

### 4. ClusterMesh connection verified

Both clusters report healthy bidirectional connectivity:

```
=== kind-clustermesh1 ===
ClusterMesh:  1/1 remote clusters ready, 0 global-services

=== kind-clustermesh2 ===
ClusterMesh:  1/1 remote clusters ready, 0 global-services
```

### 5. IPCACHE_MISS BPF signal fires correctly

When a pod on cluster2 sends traffic to an IP that has no /32 entry in the BPF ipcache
(resolves to WORLD identity), the BPF datapath emits `SIGNAL_IPCACHE_MISS`. The signal
does NOT fire for IPs that have a /32 entry (synced endpoints).

**Test**: send 10 pings to the unsynced pod IP, then 10 pings to a synced pod IP:

```
$ cilium-dbg metrics list | grep ipcache_miss
(no metric before traffic)

# After 10 pings to UNSYNCED IP (10.1.0.164):
cilium_datapath_signals_handled_total  signal=ipcache_miss status=unregistered  10.000000

# After 10 additional pings to SYNCED IP (10.1.0.215):
cilium_datapath_signals_handled_total  signal=ipcache_miss status=unregistered  10.000000
                                                                               ^^^ unchanged
```

The signal status is `unregistered` because the userspace handler Cell
(`pkg/datapath/signalhandler`) is not yet wired into the agent's cell tree — that is
intentional for the POC. The signal infrastructure works end-to-end: BPF emits the event,
the perf ring buffer delivers it, and the signal manager processes it. Wiring the resolver
to actually perform on-demand /32 lookups is the next step.

## Cleanup

```bash
kind delete cluster --name clustermesh1
kind delete cluster --name clustermesh2
```

## Notes

- **Based on v1.17** because v1.18+ added BPF requirement probes that fail on FIPS kernels (5.4.0-aws-fips). v1.17 works perfectly on these kernels.
- **Label filtering** is enabled via `CILIUM_ENABLE_SYNC_LABEL_FILTER=true` env var on the clustermesh-apiserver.
- **TLS SANs**: The `clustermesh.apiserver.tls.server.extraIpAddresses` Helm value adds the Kind node Docker IP to the server cert SANs, which is required for cross-cluster etcd connections in Kind.
- **No `cilium-cli` needed**: Cluster connection is done manually via secret patching.
