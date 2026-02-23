# Configuration and Command Line Reference

The `argocd-resource-tracker` CLI provides command-line parameters to control its behavior. This document describes all available commands and flags.

## Command: `analyze`

### Synopsis

```shell
argocd-resource-tracker analyze [flags]
```

### Description

Analyzes resource relationships and dependencies for ArgoCD applications. Can process a single application or all applications in a namespace. Outputs `resource.inclusions` YAML that can be used to configure ArgoCD's `argocd-cm` ConfigMap.

### Flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--app` | | | Application name (required for single app analysis). Supports `namespace/name` syntax (e.g., `argocd/my-app`) |
| `--app-namespace` | `-N` | `argocd` | Namespace where the application is located |
| `--all-apps` | | `false` | Analyze all applications in the namespace |
| `--strategy` | | `dynamic` | Analysis strategy: `dynamic` (OwnerRef walking) or `graph` (Cyphernetes) |
| `--namespace` | `-n` | `argocd` | ArgoCD namespace (where ArgoCD is installed) |
| `--kubeconfig` | | | Path to kubeconfig file. If not provided, uses in-cluster config or `~/.kube/config` |
| `--repo-server` | | | Repo server address. If empty, CLI will port-forward to `argocd-repo-server` service |
| `--repo-server-plaintext` | | `false` | Use unencrypted HTTP connection to repo-server (instead of TLS) |
| `--repo-server-strict-tls` | | `false` | Enable strict TLS validation for repo-server connection |
| `--repo-server-timeout-seconds` | | `60` | Timeout in seconds for repo-server RPC calls |
| `--loglevel` | | `info` | Log level: `trace`, `debug`, `info`, `warn`, or `error` |


### Strategy Options

#### Dynamic Strategy (Default)

The dynamic strategy uses the Kubernetes discovery client to list all API resource kinds (including CRDs) on the cluster and builds an in-memory parent→child cache from owner references. For each resource returned by the Argo CD repo-server (the application’s immediate children), it looks up that kind in the cache, then recursively looks up each child kind until the full dependency tree is built.

#### Graph Strategy

The Graph strategy uses the [Cyphernetes](https://cyphernet.es) library to run graph based relationship queries against the cluster. For each resource returned by the Argo CD repo-server (the application’s immediate children), it runs a Cyphernetes query to find that resource’s children, then repeats this process recursively for each child until the full dependency tree is built. 

