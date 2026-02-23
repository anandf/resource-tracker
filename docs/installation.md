# Installation and Quick Start Guide

## Installation

Argo CD Resource Tracker is currently available as a CLI tool. A Kubernetes controller mode is planned for future releases.

### Binary Installation

Download the latest release binary for your platform from the [releases page](https://github.com/anandf/resource-tracker/releases) or build from source:

```bash
git clone https://github.com/anandf/resource-tracker.git
cd resource-tracker
make build
```

The binary will be available at `dist/argocd-resource-tracker`.

## Quick Start

### Analyze a Single Application

**Basic usage (default: dynamic strategy):**
```shell
argocd-resource-tracker analyze --app argocd/my-app
```

### Analyze All Applications

```shell
argocd-resource-tracker analyze --all-apps --namespace argocd
```

### Using Graph Strategy

```shell
argocd-resource-tracker analyze --app argocd/my-app --strategy graph
```

### Output Format

The command outputs `resource.inclusions` YAML:

```yaml
resource.inclusions: |
- apigroups:
  - apps
  kinds:
  - Deployment
  - StatefulSet
  - DaemonSet
  clusters:
  - '*'
- apigroups:
  - ""
  kinds:
  - Service
  - ConfigMap
  - ServiceAccount
  - Pod
  clusters:
  - '*'
- apigroups:
  - rbac.authorization.k8s.io
  kinds:
  - Role
  - RoleBinding
  - ClusterRole
  - ClusterRoleBinding
  clusters:
  - '*'
```

This output can be copied into ArgoCD's `argocd-cm` ConfigMap to configure resource inclusions.

For complete command-line reference, see [Configuration and Command Line Reference](./reference.md).

