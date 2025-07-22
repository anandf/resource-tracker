# ArgoCD Resource Tracker

## Overview

Argo CD Resource Tracker is a tool which dynamically updates the resource inclusion settings of ArgoCD by modifying the `argocd-cm` ConfigMap. It tracks resources created by ArgoCD applications, retrieves their relationships, and ensures that ArgoCD watches only those resources managed by its applications. This reduces the number of watched resources, optimizing API server load and cache memory usage.

## Problem Statement

ArgoCD’s current implementation watches all resources in the cluster cache, leading to excessive watch connections. In Kubernetes clusters with a large number of CRDs (~200), this results in client-side throttling due to high API server load.

Static configuration settings like `resource.inclusions` and `resource.exclusions` require users to define which resource types to manage in advance, making it inflexible. This project provides a dynamic solution to watch only resources actively managed by ArgoCD applications.

## Motivation

* Reduce API server load: Minimize the number of watch connections by tracking only necessary resources.

* Lower memory usage: Reduce cache memory footprint by limiting watched resources.

* Improve flexibility: Avoid manual configuration of resource inclusions/exclusions.

## How It Works

* Continuously retrieve all ArgoCD applications.

* Process each application by fetching its target objects from the repo-server.

* Check if the resource relationships exist in resource-relation-lookup ConfigMap.

* If resource relations are missing:

  * Dynamically obtain the resource relations and update resource-relation-lookup ConfigMap and  resource inclusion settings in argocd-cm ConfigMap.

* If resource relations are present:

  * Check if the inclusion settings need an update.

  * Update the inclusion settings if necessary, otherwise skip.

## Benefits

* Dynamic resource management: Automatically tracks and manages resource inclusions settings in `argocd-cm` ConfigMap.

* Optimized API interactions: Prevents unnecessary API calls and throttling.

* Reduced operational overhead: Eliminates the need for manual resource inclusion configuration.

## Installation & Usage

See [doc](docs/installation.md) for installation and usage instructions.

This project aims to make Argo CD more efficient and scalable by reducing unnecessary resource watches. Contributions and feedback are welcome!

There are 2 methods of determining the nested children of an Argo CD Application.

### Approach 1 : Using the cyphernetes graph querying library.

This is the most efficient way of determining the nested children of one or more Argo CD Application.

If querying for all the applications, it is efficient to use the `--global` flag which is efficient and reduces the number of calls made to the Kubernetes API server.

``` shell
argocd-resource-tracker run-query --global --tracking-method label --loglevel info
```

By enabling the flag, `--update-enabled`, it is possible to calculate the `resource.inclusions` and `resource.exclusions` and update the `argocd-cm` config map present in the namespace
determined by the parameter `--argocd-namespace` (argocd by default).

```shell
argocd-resource-tracker run-query --global --tracking-method label --loglevel info --update-enabled true
```

Based on the resource tracking method configured in Argo CD, one can specify either the `label` tracking method or the `annotation` tracking method using the `--tracking-method` parameter.
```shell
argocd-resource-tracker run-query --global --tracking-method annotation --loglevel info --update-enabled true
```

### Approach 2 : Using the Argo CD repo server

In this approach, the command makes a grpc call to the Argo CD Repo server that retrieves the immediate children, then recursively going through its owner references to determine the parent-child relationships.
Any previously known relationships are not executed again to reduce the number of calls to the Kubernetes API server.

Eg:

```shell

argocd-resource-tracker run --interval 2m --loglevel info --repo-server argocd-repo-server.argocd.svc.cluster --repo-server-timeout-seconds 60 \
--repo-server-plaintext true --argocd-namespace argocd
```