# Quick Start Guide

## Prerequisites

Before installing Argo CD Resource Tracker, ensure the following:

* Enable ArgoCD Tracking via Annotation.
  
  Modify the argocd-cm ConfigMap to enable annotation-based resource tracking:
  ```
  data:
    application.resourceTrackingMethod: annotation
  ```
  Argo CD Resource Tracker relies on the argocd.argoproj.io/tracking-id annotation to dynamically construct resource relationships

* Verify Tracking Method on Managed Resources.
  
  Ensure that all live objects managed by ArgoCD have the correct tracking annotation:
  ```
  metadata:
  annotations:
    argocd.argoproj.io/tracking-id: <tracking-info>
  ```

## Installation

Follow these steps to install argocd-resource-tracker:
```
git clone https://github.com/anandf/resource-tracker.git
cd resource-tracker
kubectl apply -f manifest/install.yaml -n argocd
```

For detailed configuration options and command-line parameters, please refer to the  
[Configuration and Command Line Reference](./reference.md).