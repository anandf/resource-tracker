# Configuration and command line reference

The `argocd-resource-tracker` provides some command line parameters to control the
behaviour of its operations. The following is a list of available parameters
and their description.

## Command "version"

### Synopsis

`argocd-image-updater version`

### Description

Prints out the version of the binary and exits.

## Command "run"

### Synopsis

`argocd-resource-tracker run [flags]`

### Description

Runs the Argo CD Resource Tracker, possibly in an endless loop.

### Flags

**--interval**

Interval for how often to check for updates.
Default: 2m

**--repo-server**

Argo CD repo server address. (default "argocd-repo-server:8081")

**--repo-server-timeout-seconds**

Timeout in seconds for repo server RPC calls.
Default: 60

**--repo-server-plaintext**

If specified, use an unencrypted HTTP connection to the ArgoCD API instead of TLS.

**--repo-server-strict-tls**

If specified, enables strict TLS validation for the repo server connection.

**-loglevel**

Sets the log level. Options: trace, debug, info, warn, error.
Default: info

**--kubeconfig**

Full path to the kube client configuration (e.g., ~/.kube/config).

**--argocd-namespace**

Namespace where ArgoCD runs. If not specified, uses the current namespace.
