package dynamicbackend

import (
	"context"
	"fmt"
	"sync"

	"github.com/anandf/resource-tracker/pkg/analyzer"
	"github.com/anandf/resource-tracker/pkg/argocd"
	"github.com/anandf/resource-tracker/pkg/common"
	"github.com/anandf/resource-tracker/pkg/dynamic"
	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/emirpasic/gods/sets/hashset"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

// Backend implements the analysis using OwnerRefs and the dynamic resource graph logic.
type Backend struct{}

func NewBackend() *Backend {
	return &Backend{}
}

// Execute runs the dynamic analysis and returns grouped resource kinds.
func (b *Backend) Execute(ctx context.Context, opts analyzer.Options) (*common.GroupedResourceKinds, error) {
	logger := log.WithFields(log.Fields{
		"controllerNamespace": opts.ArgoCDNamespace,
		"strategy":            "dynamic",
		"allApps":             opts.TargetApp == "",
	})
	logger.Info("Starting dynamic analysis backend...")

	if opts.KubeConfig == nil {
		return nil, fmt.Errorf("dynamic backend: KubeConfig is nil in Options")
	}

	// Initialize ArgoCD high-level client against the control-plane cluster.
	ac, err := argocd.NewArgoCD(
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
	// Initialize the shared DynamicTracker used to discover relations across clusters.
	rt := dynamic.NewDynamicTracker(logger)
	var apps []*v1alpha1.Application
	groupedKinds := make(common.GroupedResourceKinds)
	if opts.TargetApp == "" {
		// Analyze all apps
		logger.Info("Listing all applications...")
		appsList, err := ac.ListApplications()
		if err != nil {
			return nil, err
		}
		logger.Infof("Found %d applications", len(appsList))
		apps = make([]*v1alpha1.Application, 0, len(appsList))
		for i := range appsList {
			app := appsList[i]
			apps = append(apps, &app)
			missingResources, err := ac.GetResourcesFromApplicationStatus(ctx, &app)
			if err != nil {
				logger.WithError(err).Error("Error getting missing resources from application conditions")
				continue
			}
			logger.Debugf("Found %d missing resources from application conditions", len(missingResources))
			groupedKinds.MergeResourceInfos(missingResources)
		}

	} else {
		// Analyze a single app
		logger.Infof("Getting application %q", opts.TargetApp)
		app, err := ac.GetApplication(opts.TargetApp)
		if err != nil {
			return nil, err
		}
		apps = []*v1alpha1.Application{app}
		missingResources, err := ac.GetResourcesFromApplicationStatus(ctx, app)
		if err != nil {
			return nil, err
		}
		logger.Debugf("Found %d missing resources from application conditions", len(missingResources))
		groupedKinds.MergeResourceInfos(missingResources)
	}

	// Use the v2 implementation based on errgroup for concurrency and cancellation.
	appChildren := analyzeWithDynamicTracker(opts.KubeConfigPath, ctx, apps, ac, rt, logger)
	groupedKinds.MergeResourceInfos(appChildren)
	return &groupedKinds, nil
}

// analyzeWithDynamicTracker analyzes the applications concurrently using errgroup
// and returns the computed list of resources.
func analyzeWithDynamicTracker(
	kubeconfigPath string,
	ctx context.Context,
	apps []*v1alpha1.Application,
	ac argocd.ArgoCD, // Passed in dependency
	rt *dynamic.DynamicTracker, // Passed in dependency
	logger *log.Entry,
) []*common.ResourceInfo {
	var (
		mu          sync.Mutex
		appChildren []*common.ResourceInfo
	)

	// errgroup handles concurrency, error propagation, and context cancellation.
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(4)

	for _, app := range apps {
		// Lets not terminate if we encounter an error while processing an application, we are logging the error and returning nil to continue the loop.
		// returing an error will terminate the errgroup and return the error to the caller.
		g.Go(func() error {
			appLogger := logger.WithFields(log.Fields{
				"applicationName":      app.GetName(),
				"applicationNamespace": app.GetNamespace(),
			})
			appLogger.Info("Processing application")
			server := app.Spec.Destination.Server
			if server == "" {
				var err error
				if app.Spec.Destination.Name == "" {
					err := fmt.Errorf("both destination server and name are empty")
					appLogger.WithError(err).Error("Destination missing")
					return nil
				}
				server, err = ac.GetApplicationClusterServerByName(ctx, app.Spec.Destination.Name)
				if err != nil {
					logger.WithError(err).Error("Error getting cluster by name")
					return nil
				}
			}
			appCluster, err := ac.GetAppCluster(ctx, server)
			if err != nil {
				appLogger.WithError(err).Error("Error getting cluster")
				return nil
			}
			restCfg, err := kube.RestConfigFromCluster(appCluster, kubeconfigPath)
			if err != nil {
				appLogger.WithError(err).Error("Error creating rest config")
				return nil
			}
			err = rt.SyncResourceMapper(server, restCfg)
			if err != nil {
				appLogger.WithError(err).Error("Error syncing resource mapper")
				return nil
			}
			if len(rt.ResourceMapperStore) == 0 {
				appLogger.Error("No destination clusters synced; ensure Applications have valid .spec.destination and Argo CD has access")
				return nil
			}
			childManifests, err := ac.GetApplicationChildManifests(ctx, app, kubeconfigPath, server)
			if err != nil {
				appLogger.WithError(err).Error("Error getting child manifests")
				return nil
			}
			appLogger.Debugf("Children of Argo CD application %q: %v", app.GetName(), childManifests)
			// Check if any direct resource is missing in cache
			rt.CacheMu.RLock()
			syncRequired := false
			for _, resource := range childManifests {
				k := dynamic.GetResourceKey(resource.Group, resource.Kind)
				if _, exists := rt.SharedRelationsCache[k]; !exists {
					syncRequired = true
					break
				}
			}
			rt.CacheMu.RUnlock()
			if syncRequired {
				// Ensure only one worker per cluster performs the sync; others wait.
				clusterLock := rt.GetClusterSyncLock(server)
				clusterLock.Lock()
				// Re-check under the cluster lock in case another worker already synced.
				rt.CacheMu.RLock()
				stillMissing := false
				missingResource := ""
				for _, resource := range childManifests {
					k := dynamic.GetResourceKey(resource.Group, resource.Kind)
					if _, exists := rt.SharedRelationsCache[k]; !exists {
						stillMissing = true
						missingResource = resource.String()
						break
					}
				}
				rt.CacheMu.RUnlock()
				if stillMissing {
					appLogger.WithFields(log.Fields{
						"cluster":         server,
						"missingResource": missingResource,
					}).Info("Syncing cache for missing resource")
					rt.EnsureSyncedSharedCacheOnHost(ctx, server)
					// if the direct resources are not in the cache, add them to the cache as left nodes
					rt.CacheMu.Lock()
					for _, resource := range childManifests {
						k := dynamic.GetResourceKey(resource.Group, resource.Kind)
						if _, exists := rt.SharedRelationsCache[k]; !exists {
							rt.SharedRelationsCache[k] = hashset.New()
						}
					}
					rt.CacheMu.Unlock()
				}
				clusterLock.Unlock()
			}
			relations := dynamic.GetResourceRelation(rt.SharedRelationsCache, childManifests)
			mu.Lock()
			appChildren = append(appChildren, relations...)
			mu.Unlock()
			return nil
		})
	}
	// Wait for all workers to complete.
	g.Wait()
	return appChildren
}
