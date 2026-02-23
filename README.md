# ArgoCD Resource Tracker

## Overview

Argo CD Resource Tracker is a **CLI tool** that helps manage ArgoCD's resource inclusion settings. It retrieves resources created by ArgoCD applications and their relationships, and ensures that ArgoCD watches only those resources managed by its applications. This reduces the number of watched resources, optimizing API server load and cache memory usage.

> **Note:** Currently available as a CLI tool. A Kubernetes controller mode is planned for future releases to enable continuous, automated resource tracking.

## Problem Statement

ArgoCD’s current implementation watches all resources in the cluster cache, leading to excessive watch connections. In Kubernetes clusters with a large number of CRDs (~200), this results in client-side throttling due to high API server load.

Static configuration settings like `resource.inclusions` and `resource.exclusions` require users to define which resource types to manage in advance, making it inflexible. This project provides a solution to watch only resources actively managed by ArgoCD applications.

## Motivation

* Reduce API server load: Minimize the number of watch connections by tracking only necessary resources.

* Lower memory usage: Reduce cache memory footprint by limiting watched resources.

* Improve flexibility: Avoid manual configuration of resource inclusions/exclusions.

## How It Works

* It can process a single ArgoCD application or all applications in a namespace.

* Processes each application by fetching its target objects from the ArgoCD repo-server.

* Retrieves all the relations of each ArgoCD application using one of two strategies:
     * **Dynamic (default)**: Uses owner references to recursively discover parent-child relationships.
	 * **Graph**: Uses the Cyphernetes library to query resource relationships via graph queries.
## Benefits

* Helps track and manage resource inclusions settings in `argocd-cm` ConfigMap.

* Optimised API interactions by preventing unnecessary API calls and throttling.

* Reduced operational overhead by eliminating the need for manually finding the resource relations.

## Installation & Usage

* **[Installation Guide](docs/installation.md)** - Installation instructions and quick start examples
* **[Command Reference](docs/reference.md)** - Complete command-line options and flags documentation

This project aims to make Argo CD more efficient and scalable by reducing unnecessary resource watches. Contributions and feedback are welcome!