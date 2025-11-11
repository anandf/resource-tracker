## Build and push the container image
```shell
# First, initialise the manifest
podman manifest create quay.io/anjoseph/argocd-resource-tracker:latest

# Build the image attaching them to the manifest
podman build --platform linux/amd64,linux/arm64  --manifest quay.io/anjoseph/argocd-resource-tracker:latest  .

# Finally publish the manifest
podman manifest push quay.io/anjoseph/argocd-resource-tracker:latest
```
## Create the required Argo CD Applications
```shell
oc apply -f test/testdata/openshift/applicationset.yaml -n openshift-gitops
for ns in $(oc get ns | grep test-app | awk '{print $1}'); do; oc label ns $ns argocd.argoproj.io/managed-by=openshift-gitops; done;
```

## Scale the operator to 0 replicas to avoid overwriting of argocd-cm
```shell
oc scale deploy openshift-gitops-operator-controller-manager -n openshift-gitops-operator --replicas=0
deployment.apps/openshift-gitops-operator-controller-manager scaled
```
## Start the Resource tracker deployment
```shell
oc apply -f manifest/openshift/install-graph-query.yaml
```

## Check the logs
```shell
oc logs -f  deploy/openshift-gitops-resource-tracker
```