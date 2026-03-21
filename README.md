# k8s-dns-controller

Automatically keeps Route53 DNS A records pointed at the public IPs of your
Kubernetes nodes. Designed for bare-metal or home-lab clusters with ISP-assigned
dynamic IPs where you want `*.k8s.example.com` to always resolve to your nodes.

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│  Kubernetes cluster                                     │
│                                                         │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐              │
│  │  Node A   │  │  Node B   │  │  Node C   │             │
│  │           │  │           │  │           │             │
│  │ ip-agent  │  │ ip-agent  │  │ ip-agent  │  DaemonSet  │
│  │ (host net)│  │ (host net)│  │ (host net)│             │
│  └─────┬─────┘  └─────┬─────┘  └─────┬─────┘             │
│        │              │              │                   │
│        └──────────────┼──────────────┘                   │
│                       ▼                                  │
│           ┌───────────────────────┐                      │
│           │  ConfigMap             │                      │
│           │  node-public-ips       │                      │
│           │                       │                      │
│           │  nodeA: {"ip":"…",    │                      │
│           │    "labels":{…}}      │                      │
│           │  nodeB: …             │                      │
│           └───────────┬───────────┘                      │
│                       │                                  │
│                       ▼                                  │
│           ┌───────────────────────┐                      │
│           │  dns-controller        │  Deployment (1 pod) │
│           │  (watches ConfigMap,   │                      │
│           │   reconciles Route53)  │                      │
│           └───────────┬───────────┘                      │
│                       │                                  │
└───────────────────────┼──────────────────────────────────┘
                        │
                        ▼
              ┌───────────────────┐
              │  Route53           │
              │                   │
              │  *.k8s.example.com │
              │    A 203.0.113.1  │
              │    A 203.0.113.2  │
              │    A 203.0.113.3  │
              └───────────────────┘
```

## How it works

1. **ip-agent** (DaemonSet, `hostNetwork: true`) runs on every node. Every 5
   minutes it queries multiple public IP services (icanhazip, ifconfig.me, ipify,
   ipecho) over IPv4 and requires a quorum of 2 to agree — guarding against a
   single service returning garbage. It writes the discovered IP **and the node's
   Kubernetes labels** to a shared ConfigMap as JSON, keyed by node name. Each
   agent's check interval is jittered ±25% to avoid thundering-herd effects on the
   IP providers and the ConfigMap.

2. **dns-controller** (Deployment, 1 replica) reads the ConfigMap every 30s,
   optionally filters nodes by label selector (e.g. only control-plane nodes),
   compares the IPs against the current Route53 A records, and applies an UPSERT
   if anything changed. It manages one or more record names (including wildcards).

Stale entries in the ConfigMap (e.g. from decommissioned nodes) should be removed
manually with `kubectl`. The agent intentionally does **not** remove its entry on
shutdown to avoid DNS churn during rolling updates or brief restarts.

## Setup

### 1. Build images

```bash
# Agent
docker build --target agent -t YOUR_REGISTRY/k8s-dns-controller-agent:latest .
# Controller
docker build --target controller -t YOUR_REGISTRY/k8s-dns-controller-controller:latest .
```

### 2. Configure AWS credentials

The dns-controller needs Route53 access. The recommended approach is **IAM Roles
for Service Accounts (IRSA)**, but any standard AWS credential method works.

#### Option A: IRSA (recommended for EKS)

1. Create an IAM policy from the included template:

```bash
aws iam create-policy \
  --policy-name k8s-dns-controller \
  --policy-document file://deploy/iam-policy.json
```

2. Create an IAM role with a trust policy that allows your cluster's OIDC
   provider to assume it. Replace `ACCOUNT`, `REGION`, `OIDC_ID`, and
   `NAMESPACE` with your values:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::ACCOUNT:oidc-provider/oidc.eks.REGION.amazonaws.com/id/OIDC_ID"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "oidc.eks.REGION.amazonaws.com/id/OIDC_ID:sub": "system:serviceaccount:NAMESPACE:k8s-dns-controller"
        }
      }
    }
  ]
}
```

3. Attach the policy to the role:

```bash
aws iam attach-role-policy \
  --role-name k8s-dns-controller \
  --policy-arn arn:aws:iam::ACCOUNT:policy/k8s-dns-controller
```

4. Annotate the Kubernetes ServiceAccount so the AWS SDK picks up the role
   automatically:

```bash
kubectl -n kube-system annotate sa k8s-dns-controller \
  eks.amazonaws.com/role-arn=arn:aws:iam::ACCOUNT:role/k8s-dns-controller
```

#### Option B: Instance profile

If your nodes already have an instance profile with Route53 access (common in
self-managed clusters), no extra configuration is needed — the AWS SDK will use
it automatically.

#### Option C: Static credentials (not recommended)

Mount a Secret with `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` as env vars
on the dns-controller Deployment. See the commented-out volume mount in
`deploy/deployment.yaml`.

### 3. Deploy

Edit the `CHANGEME` values in the manifests, then:

```bash
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/daemonset.yaml
kubectl apply -f deploy/deployment.yaml
```

### 4. Verify

```bash
# Check agent logs
kubectl -n kube-system logs -l app.kubernetes.io/name=ip-agent

# Check ConfigMap
kubectl -n kube-system get configmap node-public-ips -o yaml

# Check controller logs
kubectl -n kube-system logs -l app.kubernetes.io/name=dns-controller

# Verify DNS
dig +short '*.k8s.example.com'
```

## Configuration

### ip-agent (DaemonSet)

| Env var              | Default       | Description                                        |
|----------------------|---------------|----------------------------------------------------|
| `NODE_NAME`          | *required*    | Set via downward API (`spec.nodeName`)              |
| `NAMESPACE`          | `kube-system` | Namespace of the shared ConfigMap                   |
| `CHECK_INTERVAL`     | `5m`          | How often to recheck the public IP (jittered ±25%) |
| `EXTRA_IP_PROVIDERS` | (none)        | Comma-separated URLs prepended to the provider list |

### dns-controller (Deployment)

| Env var              | Default       | Description                                              |
|----------------------|---------------|---------------------------------------------------------|
| `HOSTED_ZONE_ID`     | *required*    | Route53 hosted zone ID                                   |
| `RECORD_NAMES`       | *required*    | Comma-separated DNS names to manage (e.g. `*.k8s.example.com`) |
| `NAMESPACE`          | `kube-system` | Namespace of the shared ConfigMap                        |
| `DNS_TTL`            | `60s`         | TTL for A records                                        |
| `RECONCILE_INTERVAL` | `30s`         | How often to poll the ConfigMap and reconcile             |
| `NODE_SELECTOR`      | (none)        | Label selector to filter which nodes' IPs are used (see below) |

#### Node selector examples

| Selector | Effect |
|----------|--------|
| `node-role.kubernetes.io/control-plane` | Only control-plane / API server nodes |
| `!node-role.kubernetes.io/control-plane` | Only worker nodes (exclude control-plane) |
| `topology.kubernetes.io/zone=us-east-1a` | Only nodes in a specific zone |
| `team=backend,env=prod` | Multiple requirements (AND logic) |

Uses standard [Kubernetes label selector syntax](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors).
When unset, all node IPs are included.

## Design decisions

- **ConfigMap as database**: Avoids deploying etcd/postgres for what is a tiny
  amount of data. Each node entry is stored as JSON containing the IP and the
  node's Kubernetes labels, enabling the controller to filter by role, zone, or
  any other label without needing direct access to the Kubernetes node API. The
  store handles legacy plain-IP entries gracefully for backwards compatibility.

- **Node label filtering**: The controller supports standard Kubernetes label
  selector syntax via `NODE_SELECTOR`. This lets you point DNS only at
  control-plane nodes, workers, a specific zone, or any combination — without
  running separate controller instances.

- **hostNetwork: true**: The agent must make outbound HTTP requests from the
  node's actual network stack, not through kube-proxy or a CNI overlay, to get
  the correct public IP.

- **IPv4 only**: The HTTP client forces `tcp4` connections so whoami services
  always return the node's public IPv4 address. This avoids getting an IPv6
  address that can't be used in an A record.

- **Quorum on IP discovery**: Protects against a single whoami service returning
  a cached/wrong IP. The default quorum of 2 means at least 2 of 4 services must
  agree.

- **Safety: won't delete all records**: If the ConfigMap is empty (e.g. all agents
  crashed), the controller logs a warning and does nothing rather than removing
  all A records.

- **No cleanup on shutdown**: The agent does not remove its ConfigMap entry when
  it stops. This avoids unnecessary DNS churn during rolling updates or brief
  restarts. Stale entries from permanently removed nodes should be cleaned up
  manually.
