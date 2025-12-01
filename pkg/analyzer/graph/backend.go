package graphbackend

import (
	"context"
	"fmt"

	"github.com/anandf/resource-tracker/pkg/analyzer"
	"github.com/anandf/resource-tracker/pkg/argocd"
	"github.com/anandf/resource-tracker/pkg/common"
	"github.com/anandf/resource-tracker/pkg/graph"
	log "github.com/sirupsen/logrus"
)

// Backend implements the analysis using the Cyphernetes graph query engine.
type Backend struct{}

func NewBackend() *Backend {
	return &Backend{}
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
		return nil, fmt.Errorf("graph backend: KubeConfig is nil in Options")
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

	qs, err := graph.NewQueryServer(opts.KubeConfig, trackingMethod, true)
	if err != nil {
		return nil, err
	}

	var allAppChildren []common.ResourceInfo
	if opts.TargetApp != "" {
		if opts.TargetApp == "" {
			return nil, fmt.Errorf("global graph query requires TargetApp to be set")
		}
		appLogger := logger.WithField("applicationName", opts.TargetApp)
		appLogger.Infof("Processing application")
		argoApp, err := argoCDClient.GetApplication(opts.TargetApp)
		if err != nil {
			return nil, err
		}
		appChildren, err := argoCDClient.GetApplicationChildManifests(ctx, argoApp, opts.KubeConfigPath, "")
		if err != nil {
			appLogger.WithError(err).Error("Error getting application children")
			return nil, err
		}
		appLogger.Debugf("Children of Argo CD application %q: %v", argoApp.Name, appChildren)
		for _, appChild := range appChildren {
			childResources, err := qs.GetNestedChildResources(appChild)
			if err != nil {
				appLogger.WithError(err).Error("Error getting nested child resources")
				return nil, err
			}
			for childResource := range childResources {
				allAppChildren = append(allAppChildren, childResource)
			}
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
			appChildren, err := argoCDClient.GetApplicationChildManifests(ctx, &argoApp, opts.KubeConfigPath, "")
			if err != nil {
				appLogger.WithError(err).Error("Error getting application children")
				continue
			}
			for _, appChild := range appChildren {
				childResources, err := qs.GetNestedChildResources(appChild)
				if err != nil {
					appLogger.WithError(err).Error("Error getting nested child resources")
					continue
				}
				for childResource := range childResources {
					allAppChildren = append(allAppChildren, childResource)
				}
				appLogger.Debugf("Children of Argo CD application %q: %v", argoApp.Name, childResources)
			}
		}
	}
	groupedKinds := make(common.GroupedResourceKinds)
	groupedKinds.MergeResourceInfos(allAppChildren)
	// Merge missing resources reported via Application conditions.
	missingResources, err := argoCDClient.GetAllMissingResources()
	if err != nil {
		return nil, err
	}
	logger.Debugf("Found %d missing resources from app conditions", len(missingResources))
	groupedKinds.MergeResourceInfos(missingResources)
	return &groupedKinds, nil
}
