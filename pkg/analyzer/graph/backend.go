package graphbackend

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	log "github.com/sirupsen/logrus"

	"github.com/anandf/resource-tracker/pkg/analyzer"
	"github.com/anandf/resource-tracker/pkg/argocd"
	"github.com/anandf/resource-tracker/pkg/common"
	"github.com/anandf/resource-tracker/pkg/graph"
	"github.com/anandf/resource-tracker/pkg/kube"
)

// It caches one QueryServer per destination cluster.
type Backend struct {
	mu           sync.Mutex
	queryServers map[string]*graph.QueryServer
}

func NewBackend() *Backend {
	return &Backend{
		mu:           sync.Mutex{},
		queryServers: make(map[string]*graph.QueryServer),
	}
}

// Execute performs a graph-based analysis and returns grouped resource kinds.
func (b *Backend) Execute(ctx context.Context, opts analyzer.Options) (*common.GroupedResourceKinds, error) {
	logger := log.WithFields(log.Fields{
		"controllerNamespace":  opts.ArgoCDNamespace,
		"strategy":             "graph",
		"applicationNamespace": opts.TargetAppNamespace,
	})
	logger.Info("Starting graph (Cyphernetes) analysis backend...")

	if opts.KubeConfig == nil {
		return nil, errors.New("graph backend: KubeConfig is nil in Options")
	}

	// Initialize ArgoCD client against the control-plane cluster.
	argoCDClient, err := argocd.NewArgoCD(
		opts.KubeConfig,
		opts.ArgoCDNamespace,
		opts.TargetAppNamespace,
		opts.RepoServerAddress,
		opts.RepoServerTimeoutSeconds,
		opts.RepoServerPlaintext,
		opts.RepoServerStrictTLS,
	)
	if err != nil {
		return nil, err
	}
	trackingMethod, err := argoCDClient.GetTrackingMethod()
	if err != nil {
		return nil, err
	}

	var allAppChildren []*common.ResourceInfo
	if opts.TargetApp != "" {
		appLogger := logger.WithField("applicationName", opts.TargetApp)
		appLogger.Infof("Processing application")
		argoApp, err := argoCDClient.GetApplication(opts.TargetApp)
		if err != nil {
			// If the application itself cannot be fetched, fail fast.
			return nil, err
		}

		// Try to resolve and traverse the destination cluster; on failure just log
		// and fall back to status-based resources.
		if qs, err := b.getQueryServerForApp(ctx, argoCDClient, argoApp, opts.KubeConfigPath, trackingMethod, appLogger); err != nil {
			appLogger.WithError(err).Error("Error getting query server for destination cluster")
		} else {
			appChildren, err := argoCDClient.GetApplicationChildManifests(ctx, argoApp)
			if err != nil {
				appLogger.WithError(err).Error("Error getting application children")
			} else {
				appLogger.Debugf("Children of Argo CD application %q: %v", argoApp.Name, appChildren)
				for _, appChild := range appChildren {
					childResources, err := qs.GetNestedChildResources(appChild)
					if err != nil {
						appLogger.WithError(err).Error("Error getting nested child resources")
						continue
					}
					for childResource := range childResources {
						allAppChildren = append(allAppChildren, &childResource)
					}
				}
			}
		}

		// Always try to augment with resources inferred from Application.status,
		// even if graph traversal failed.
		resources, err := argoCDClient.GetResourcesFromApplicationStatus(argoApp)
		if err != nil {
			appLogger.WithError(err).Error("Error getting resources from application status")
		} else {
			allAppChildren = append(allAppChildren, resources...)
		}
	} else {
		argoApps, err := argoCDClient.ListApplications()
		if err != nil {
			return nil, err
		}
		logger.Infof("Found %d applications", len(argoApps))
		for _, argoApp := range argoApps {
			appLogger := logger.WithField("applicationName", argoApp.Name)
			appLogger.Info("Processing application")
			appLogger.Debugf("Querying Argo CD application %q", argoApp.Name)
			appChildren, err := argoCDClient.GetApplicationChildManifests(ctx, &argoApp)
			if err != nil {
				appLogger.WithError(err).Error("Error getting application children")
			}
			// Try to resolve and traverse the destination cluster; on failure just
			// log and fall back to status-based resources.
			if qs, err := b.getQueryServerForApp(ctx, argoCDClient, &argoApp, opts.KubeConfigPath, trackingMethod, logger); err != nil {
				appLogger.WithError(err).Error("Error getting query server for application")
			} else {
				for _, appChild := range appChildren {
					childResources, err := qs.GetNestedChildResources(appChild)
					if err != nil {
						appLogger.WithError(err).Error("Error getting nested child resources")
						continue
					}
					for childResource := range childResources {
						allAppChildren = append(allAppChildren, &childResource)
					}
					appLogger.Debugf("Children of Argo CD application %q: %v", argoApp.Name, childResources)
				}
			}
			// Always try to augment with resources inferred from Application.status,
			// even if graph traversal failed.
			resources, err := argoCDClient.GetResourcesFromApplicationStatus(&argoApp)
			if err != nil {
				appLogger.WithError(err).Error("Error getting resources from application status")
				continue
			}
			allAppChildren = append(allAppChildren, resources...)
		}
	}
	groupedKinds := make(common.GroupedResourceKinds)
	groupedKinds.MergeResourceInfos(allAppChildren)
	return &groupedKinds, nil
}

// getQueryServerForApp resolves the destination cluster for the given Argo CD
// Application and returns a cached QueryServer for that cluster, creating it
// if necessary.
func (b *Backend) getQueryServerForApp(
	ctx context.Context,
	argoCDClient argocd.ArgoCD,
	app *v1alpha1.Application,
	kubeConfigPath string,
	trackingMethod string,
	logger *log.Entry,
) (*graph.QueryServer, error) {
	// Determine the destination server for this application.
	server := app.Spec.Destination.Server
	if server == "" {
		if app.Spec.Destination.Name == "" {
			return nil, fmt.Errorf("both destination server and name are empty for application %q", app.Name)
		}
		var err error
		server, err = argoCDClient.GetApplicationClusterServerByName(ctx, app.Spec.Destination.Name)
		if err != nil {
			return nil, fmt.Errorf("error getting application cluster server by name %q: %w", app.Spec.Destination.Name, err)
		}
	}

	// Check cache first.
	b.mu.Lock()
	if qs, ok := b.queryServers[server]; ok {
		b.mu.Unlock()
		return qs, nil
	}
	b.mu.Unlock()

	// Fetch the Argo CD Cluster object and build a rest.Config for the
	// destination cluster.
	cluster, err := argoCDClient.GetAppCluster(ctx, server)
	if err != nil {
		return nil, fmt.Errorf("failed to get destination cluster %q: %w", server, err)
	}
	restCfg, err := kube.RestConfigFromCluster(cluster, kubeConfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to build rest.Config for cluster %q: %w", server, err)
	}

	qs, err := graph.NewQueryServer(restCfg, trackingMethod)
	if err != nil {
		return nil, fmt.Errorf("failed to create query server for cluster %q: %w", server, err)
	}

	b.mu.Lock()
	b.queryServers[server] = qs
	b.mu.Unlock()

	logger.WithField("cluster", server).Debug("Created new QueryServer for destination cluster")
	return qs, nil
}
