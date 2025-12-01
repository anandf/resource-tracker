package analyzer

import (
	"context"

	"github.com/anandf/resource-tracker/pkg/common"
	"k8s.io/client-go/rest"
)

// Options holds the configuration required to run an analysis.
type Options struct {
	// KubeConfig is the Kubernetes REST config for talking to the Argo CD control plane.
	// In the CLI this is typically loaded from a kubeconfig file; in an Operator it
	// would usually come from rest.InClusterConfig().
	KubeConfig *rest.Config

	// KubeConfigPath is the original kubeconfig path used by the CLI to load the KubeConfig.
	KubeConfigPath string

	// ArgoCDNamespace is where Argo CD is running.
	ArgoCDNamespace string

	// TargetApp is the specific app to analyze. If empty, analyze all applications.
	TargetApp string

	// TargetAppNamespace is the namespace of the target application.
	TargetAppNamespace string

	// RepoServerAddress is the address of the Argo CD repo-server (host:port).
	RepoServerAddress string

	// RepoServerPlaintext controls whether the repo-server connection uses
	// plain HTTP instead of TLS.
	RepoServerPlaintext bool

	// RepoServerStrictTLS controls whether strict TLS verification is enabled
	// for the repo-server connection.
	RepoServerStrictTLS bool

	// RepoServerTimeoutSeconds is the timeout for repo-server RPC calls.
	RepoServerTimeoutSeconds int
}

// Backend is the common interface that both CLI and Operator code can use.
type Backend interface {
	Execute(ctx context.Context, opts Options) (*common.GroupedResourceKinds, error)
}
