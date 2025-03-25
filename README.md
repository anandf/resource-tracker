# ArgoCD Resource Tracker

## Overview

Argo CD Resource Tracker is a tool which dynamically updates the resource inclusion settings of ArgoCD by modifying the argocd-cm ConfigMap. It tracks resources created by ArgoCD applications, retrieves their relationships, and ensures that ArgoCD watches only those resources managed by its applications. This reduces the number of watched resources, optimizing API server load and cache memory usage.

## Problem Statement

ArgoCD’s current implementation watches all resources in the cluster cache, leading to excessive watch connections. In OpenShift or Kubernetes clusters with a large number of CRDs (~200), this results in client-side throttling due to high API server load.

Static configuration settings like resource.inclusions and resource.exclusions require users to define which resource types to manage in advance, making it inflexible. This project provides a dynamic solution to watch only resources actively managed by ArgoCD applications.

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

* Dynamic resource management: Automatically tracks and manages resource inclusions settings in argocd-cm ConfigMap.

* Optimized API interactions: Prevents unnecessary API calls and throttling.

* Reduced operational overhead: Eliminates the need for manual resource inclusion configuration.

## Installation & Usage

See [doc](docs/installation.md) for installation and usage instructions.

This project aims to make Argo CD more efficient and scalable by reducing unnecessary resource watches. Contributions and feedback are welcome!