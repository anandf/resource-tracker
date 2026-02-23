package dynamicbackend

import (
	"context"
	"errors"
	"sync"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/emirpasic/gods/sets/hashset"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"

	"github.com/anandf/resource-tracker/pkg/analyzer"
	"github.com/anandf/resource-tracker/pkg/argocd"
	"github.com/anandf/resource-tracker/pkg/common"
	"github.com/anandf/resource-tracker/pkg/dynamic"
	"github.com/anandf/resource-tracker/pkg/kube"
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
		return nil, errors.New("dynamic backend: KubeConfig is nil in Options")
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
			missingResources, err := ac.GetResourcesFromApplicationStatus(&app)
			if err != nil {
				logger.WithError(err).Error("Error getting missing resources from application conditions")
				continue
			}
			logger.Debugf("Found %d missing resources from application conditions", len(missingResources))
			groupedKinds.MergeResourceInfos(missingResources)
		}
	} else { // Analyze a single app
		logger.Infof("Getting application %q", opts.TargetApp)
		app, err := ac.GetApplication(opts.TargetApp)
		if err != nil {
			return nil, err
		}
		apps = []*v1alpha1.Application{app}
		missingResources, err := ac.GetResourcesFromApplicationStatus(app)
		if err != nil {
			return nil, err
		}
		logger.Debugf("Found %d missing resources from application conditions", len(missingResources))
		groupedKinds.MergeResourceInfos(missingResources)
	}

	// Use the v2 implementation based on errgroup for concurrency and cancellation.
	appChildren := analyzeWithDynamicTracker(ctx, opts.KubeConfigPath, apps, ac, rt, logger)
	groupedKinds.MergeResourceInfos(appChildren)
	return &groupedKinds, nil
}

// analyzeWithDynamicTracker analyzes the applications concurrently using errgroup
// and returns the computed list of resources.
func analyzeWithDynamicTracker(
	ctx context.Context,
	kubeconfigPath string,
	apps []*v1alpha1.Application,
	ac argocd.ArgoCD,
	rt *dynamic.DynamicTracker,
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
			// Check for context cancellation
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			appLogger := logger.WithFields(log.Fields{
				"applicationName":      app.GetName(),
				"applicationNamespace": app.GetNamespace(),
			})
			appLogger.Info("Processing application")
			server := app.Spec.Destination.Server
			if server == "" {
				if app.Spec.Destination.Name == "" {
					err := errors.New("both destination server and name are empty")
					appLogger.WithError(err).Error("Destination missing")
					return nil
				}
				var err error
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
			// Check if the mapper for this specific server was created successfully
			rt.CacheMu.RLock()
			mapperExists := rt.ResourceMapperStore[server] != nil
			rt.CacheMu.RUnlock()
			if !mapperExists {
				appLogger.Error("Resource mapper for cluster was not created; ensure Applications have valid .spec.destination and Argo CD has access")
				return nil
			}
			childManifests, err := ac.GetApplicationChildManifests(ctx, app)
			if err != nil {
				appLogger.WithError(err).Error("Error getting child manifests")
				return nil
			}
			appLogger.Debugf("Children of Argo CD application %q: %v", app.GetName(), childManifests)
			// Check if any direct resource is missing in cache
			rt.CacheMu.RLock()
			missingKeys := make([]string, 0)
			for _, resource := range childManifests {
				k := dynamic.GetResourceKey(resource.Group, resource.Kind)
				if _, exists := rt.SharedRelationsCache[k]; !exists {
					missingKeys = append(missingKeys, k)
				}
			}
			rt.CacheMu.RUnlock()
			if len(missingKeys) > 0 {
				// Ensure only one worker per cluster performs the sync; others wait.
				// The cluster lock prevents multiple goroutines from syncing the same cluster concurrently.
				clusterLock := rt.GetClusterSyncLock(server)
				clusterLock.Lock()
				// Re-check under the cluster lock: another goroutine might have already synced
				// the cache while we were waiting for the lock, making our sync unnecessary.
				rt.CacheMu.RLock()
				stillMissingKeys := make([]string, 0)
				for _, k := range missingKeys {
					if _, exists := rt.SharedRelationsCache[k]; !exists {
						stillMissingKeys = append(stillMissingKeys, k)
					}
				}
				rt.CacheMu.RUnlock()
				if len(stillMissingKeys) > 0 {
					appLogger.WithFields(log.Fields{
						"cluster": server,
					}).Info("Syncing cache for missing resources")
					rt.EnsureSyncedSharedCacheOnHost(ctx, server)
					// Add direct resources as leaf nodes (empty children set) if they're still not in cache
					// This ensures they're tracked even if they have no children or weren't discovered during sync
					rt.CacheMu.Lock()
					for _, k := range stillMissingKeys {
						if _, exists := rt.SharedRelationsCache[k]; !exists {
							rt.SharedRelationsCache[k] = hashset.New()
						}
					}
					rt.CacheMu.Unlock()
				}
				clusterLock.Unlock()
			}
			// Read cache with lock to ensure consistency during DFS traversal
			rt.CacheMu.RLock()
			relations := dynamic.GetResourceRelation(rt.SharedRelationsCache, childManifests)
			rt.CacheMu.RUnlock()
			mu.Lock()
			appChildren = append(appChildren, relations...)
			mu.Unlock()
			return nil
		})
	}
	// Wait for all workers to complete.
	err := g.Wait()
	if err != nil {
		logger.WithError(err).Error("Error analyzing applications")
	}
	return appChildren
}
