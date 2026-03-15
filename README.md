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
│           │  nodeA: 203.0.113.1   │                      │
│           │  nodeB: 203.0.113.2   │                      │
│           │  nodeC: 203.0.113.3   │                      │
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

1. **ip-agent** (DaemonSet, `hostNetwork: true`) runs on every node. Every 60s it
   queries multiple public IP services (icanhazip, ifconfig.me, ipify, ipecho)
   and requires a quorum of 2 to agree — guarding against a single service
   returning garbage. It writes the result to a shared ConfigMap keyed by node
   name.

2. **dns-controller** (Deployment, 1 replica) reads the ConfigMap every 30s,
   compares the IPs against the current Route53 A records, and applies an UPSERT
   if anything changed. It manages one or more record names (including wildcards).

3. On shutdown, the agent removes its own entry from the ConfigMap so the
   controller stops advertising the IP of a dead node.

## Setup

### 1. Build images

```bash
# Agent
docker build --target agent -t YOUR_REGISTRY/k8s-dns-controller-agent:latest .
# Controller
docker build --target controller -t YOUR_REGISTRY/k8s-dns-controller-controller:latest .
```

### 2. Create IAM policy

Apply `deploy/iam-policy.json` and attach it to the controller's identity.
With IRSA, annotate the ServiceAccount:

```bash
kubectl -n kube-system annotate sa k8s-dns-controller \
  eks.amazonaws.com/role-arn=arn:aws:iam::ACCOUNT:role/k8s-dns-controller
```

For non-EKS clusters, use any AWS credential method (env vars, instance profile,
mounted secret).

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
| `CHECK_INTERVAL`     | `60s`         | How often to recheck the public IP                  |
| `EXTRA_IP_PROVIDERS` | (none)        | Comma-separated URLs prepended to the provider list |

### dns-controller (Deployment)

| Env var              | Default       | Description                                              |
|----------------------|---------------|---------------------------------------------------------|
| `HOSTED_ZONE_ID`     | *required*    | Route53 hosted zone ID                                   |
| `RECORD_NAMES`       | *required*    | Comma-separated DNS names to manage (e.g. `*.k8s.example.com`) |
| `NAMESPACE`          | `kube-system` | Namespace of the shared ConfigMap                        |
| `DNS_TTL`            | `60s`         | TTL for A records                                        |
| `RECONCILE_INTERVAL` | `30s`         | How often to poll the ConfigMap and reconcile             |

## Design decisions

- **ConfigMap as database**: Avoids deploying etcd/postgres for what is a tiny
  amount of data. A 100-node cluster stores ~2KB. If you already run postgres and
  want durability beyond what the k8s API server provides, swapping out the
  `store` package is straightforward.

- **hostNetwork: true**: The agent must make outbound HTTP requests from the
  node's actual network stack, not through kube-proxy or a CNI overlay, to get
  the correct public IP.

- **Quorum on IP discovery**: Protects against a single whoami service returning
  a cached/wrong IP. The default quorum of 2 means at least 2 of 4 services must
  agree.

- **Safety: won't delete all records**: If the ConfigMap is empty (e.g. all agents
  crashed), the controller logs a warning and does nothing rather than removing
  all A records.

- **Agent cleanup on shutdown**: Graceful termination removes the node's entry,
  so the controller can stop advertising an IP that's no longer serving traffic.
