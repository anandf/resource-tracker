# Configuration and command line reference

The `argocd-resource-tracker` provides some command line parameters to control the
behaviour of its operations. The following is a list of available parameters
and their description.

## Command "version"

### Synopsis

`argocd-image-updater version`

### Description

Prints out the version of the binary and exits.

## Command "run-query"

### Synopsis

`argocd-resource-tracker run-query [flags]`

### Description

Finds the resource kinds that Argo CD manages by running a [Cyphernetes](https://cyphernet.es) graph query using either label or annotation tracking.

### Flags

**--interval**

Interval for how often to check for updates.
Default: 5m

**--loglevel**

Sets the log level. Options: trace, debug, info, warn, error.
Default: info

**--tracking-method**

Sets the log level. Options: trace, debug, info, warn, error.
Default: label

**--kubeconfig**

Full path to the kube client configuration (e.g., ~/.kube/config).

**--app-name**

Name of the app whose children needs to be queried. If left empty, all Argo CD Applications would be considered for the query.
Default: ""

**--app-namespace**

Namespace in which the applications will be queried and nested children would be considered. If left empty, all namespaces are considered for the query.
Default: ""

**--argocd-namespace**

Namespace in which the Argo CD control plane components are running and the `argocd-cm` config map is present
Default: "argocd"

**--global**

An efficient way of querying children when children for all applications across all namespaces needs to be queried.
Default: "true"
Allowed Values: "true" or "false"

**--direct-update**

If this flag is enabled, then the `resource.inclusions` and `resource.exclusions` properties in `argocd-cm` config map gets updated automatically.
Default: "false"
Allowed Values: "true" or "false"

**--once**

If this flag is enabled, then the command would be run only once and if disabled, the command would run continuously in a loop.
Default: "false"
Allowed Values: "true" or "false"

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
