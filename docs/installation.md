# Quick Start Guide

## Installation

It is required to run Argo CD Resource Tracker in the same Kubernetes namespace where Argo CD is installed.

### Follow these steps to install argocd-resource-tracker:
```
git clone https://github.com/anandf/resource-tracker.git
cd resource-tracker
kubectl apply -f manifest/install.yaml -n argocd
```

## resource-relation-lookup ConfigMap

Argo CD Resource Tracker utilizes the resource-relation-lookup ConfigMap to optimize resource tracking and reduce unnecessary API queries. This ConfigMap stores discovered parent-child relationships between Kubernetes resources, allowing Argo CD Resource Tracker to efficiently track dependencies without repeatedly querying the API server.

### resource-relation-lookup ConfigMap Structure

```
apiVersion: v1
data:
  apps_DaemonSet: apps_ControllerRevision,core_Pod
  apps_Deployment: apps_ReplicaSet
  apps_ReplicaSet: core_Pod
  apps_StatefulSet: apps_ControllerRevision,core_Pod
  core_Node: coordination.k8s.io_Lease,core_Pod
  core_Service: discovery.k8s.io_EndpointSlice
kind: ConfigMap
metadata:
  name: resource-relation-lookup
  namespace: argocd
```

### How Argo CD Resource Tracker Uses the resource-relation-lookup ConfigMap

* When processing an Argo CD application, the Resource Tracker first checks the resource-relation-lookup ConfigMap to determine if the resource relationships have already been discovered.

* If the necessary relationships exist in the ConfigMap, Argo CD directly utilizes them, avoiding redundant API queries.

* If no relation is found in the ConfigMap, the Argo CD Resource Tracker queries all objects in the API server and, using owner references, discovers the relationships dynamically.

* Once new relationships are identified, they are added to the ConfigMap to ensure that subsequent applications do not need to query the API server again, reducing API load and improving performance.

### Cluster-wide Inclusion Pattern
To ensure Argo CD watches all relevant resources across multiple clusters, the clusters: ['*'] wildcard is used in the resource.inclusions setting:
```
- apiGroups:
  - apps
  kinds:
  - ReplicaSet
  - Deployment
  clusters:
  - '*'

```
This wildcard enables inclusion settings to apply globally across all clusters managed by Argo CD. Due to the ConfigMap size limitation (1MB), defining inclusion rules per cluster individually is not scalable. Using the wildcard reduces redundancy and keeps the configuration compact.

***Trade-off:***

While this ensures that all necessary resources managed by Argo CD are watched, it may also include a few unrelated resources in clusters where those relationships don’t exist.

**Example:**

**In Cluster A:**
- **Resource Kind:** `Deployment`
  - **Children:** `ReplicaSets`, `Services`

**In Cluster B:**
- **Resource Kind:** `Deployment`
  - **Children:** `ReplicaSets`, `Services`, `Secrets`

Since resource relationships can differ across clusters, using `clusters: ['*']` ensures that all possible resource relationships are covered, even when they vary across clusters, without the need for maintaining separate configurations for each cluster.


For detailed configuration options and command-line parameters, please refer to the  
[Configuration and Command Line Reference](./reference.md).

