package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/anandf/resource-tracker/pkg/argocd"
	"github.com/anandf/resource-tracker/pkg/common" // <-- IMPORT COMMON
	"github.com/emirpasic/gods/sets/hashset"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/util/db"
	"github.com/argoproj/argo-cd/v3/util/settings"

	"github.com/anandf/resource-tracker/pkg/resourcegraph"
	"k8s.io/client-go/tools/clientcmd"
	clientauthapi "k8s.io/client-go/tools/clientcmd/api"
)

// DynamicBackend implements the analysis using OwnerRefs and resource graph logic.
type DynamicBackend struct {
	tracker    *DynamicTracker
	argoCD     argocd.ArgoCD
	restConfig *rest.Config
}

// NewDynamicBackend initializes ArgoCD client, k8s clientset and the ResourceTracker.
func NewDynamicBackend(cfg *QueryConfig) (*DynamicBackend, error) {
	// First try in-cluster config
	restConfig, err := rest.InClusterConfig()
	if err != nil && !errors.Is(err, rest.ErrNotInCluster) {
		return nil, fmt.Errorf("failed to create config: %v", err)
	}
	// Fall back to kubeconfig
	if restConfig == nil {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		loadingRules.ExplicitPath = cfg.kubeConfig
		configOverrides := &clientcmd.ConfigOverrides{}
		kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
		restConfig, err = kubeConfig.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to create config: %v", err)
		}
	}

	// ArgoCD high-level client
	ac, err := argocd.NewArgoCD(restConfig, cfg.argocdNamespace, cfg.applicationNamespace)
	if err != nil {
		return nil, err
	}
	// Kube clientset for settings/db
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	rt := NewResourceTracker(ac, clientset, cfg.argocdNamespace)
	// Use provided kubeconfig path for potential in-cluster redirection
	rt.kubeconfig = cfg.kubeConfig
	return &DynamicBackend{
		tracker:    rt,
		argoCD:     ac,
		restConfig: restConfig,
	}, nil
}

// Execute now returns the computed kinds and an error, matching the interface.
func (b *DynamicBackend) Execute(cfg *QueryConfig) (*common.GroupedResourceKinds, error) {
	logger := log.WithFields(log.Fields{
		"controllerNamespace":  cfg.argocdNamespace,
		"strategy":             "dynamic",
		"applicationName":      cfg.applicationName,
		"allApps":              cfg.allApps,
		"applicationNamespace": cfg.applicationNamespace,
	})
	logger.Info("Starting query executor...")
	var apps []*v1alpha1.Application
	if cfg.allApps {
		logger.Info("Listing all applications...")
		appsList, err := b.argoCD.ListApplications()
		if err != nil {
			return nil, err
		}
		logger.Infof("Found %d applications", len(appsList))
		apps = make([]*v1alpha1.Application, 0, len(appsList))
		for i := range appsList {
			app := appsList[i]
			apps = append(apps, &app)
		}
	} else {
		logger.Info("Listing single application")
		app, err := b.argoCD.GetApplication(cfg.applicationName)
		if err != nil {
			return nil, err
		}
		apps = []*v1alpha1.Application{app}
	}
	appChildren, err := b.tracker.AnalyzeWithDynamicTracker(b.restConfig, apps, logger)
	if err != nil {
		return nil, err
	}
	groupedKinds := make(common.GroupedResourceKinds)
	groupedKinds.MergeResourceInfos(appChildren)
	return &groupedKinds, nil
}

// TODO: Can we move this to the pkg/resourcegraph package?
// DynamicTracker handles the analysis of ArgoCD application resources
type DynamicTracker struct {
	db                  db.ArgoDB
	argoClient          argocd.ArgoCD
	resourceMapperStore map[string]*resourcegraph.ResourceMapper
	kubeconfig          string
	// shared cache across ALL clusters: parentKey -> set(childKey)
	sharedRelationsCache map[string]*hashset.Set
	cacheMu              sync.RWMutex
	// per-cluster sync locks to avoid concurrent resyncs for the same cluster
	syncLocks map[string]*sync.Mutex
}

// NewResourceTracker creates a new resource tracker instance
func NewResourceTracker(argoClient argocd.ArgoCD, clientset kubernetes.Interface, controllerNamespace string) *DynamicTracker {

	settingsMgr := settings.NewSettingsManager(context.Background(), clientset, controllerNamespace)
	dbInstance := db.NewDB(controllerNamespace, settingsMgr, clientset)

	return &DynamicTracker{
		argoClient:           argoClient,
		db:                   dbInstance,
		resourceMapperStore:  make(map[string]*resourcegraph.ResourceMapper),
		sharedRelationsCache: make(map[string]*hashset.Set),
		syncLocks:            make(map[string]*sync.Mutex),
	}
}

// AnalyzeWithDynamicTracker analyzes the applications and returns the computed map
func (rt *DynamicTracker) AnalyzeWithDynamicTracker(config *rest.Config, apps []*v1alpha1.Application, logger *log.Entry) ([]common.ResourceInfo, error) {
	appChildren := []common.ResourceInfo{}
	addToAppChildren := func(resourceInfo common.ResourceInfo) {
		appChildren = append(appChildren, resourceInfo)
	}
	type result struct{ err error }
	jobs := make(chan *v1alpha1.Application)
	results := make(chan result)
	// Bounded workers
	workerCount := 4
	var wg sync.WaitGroup
	report := func(err error) { results <- result{err: err} }
	wg.Add(workerCount)
	for range workerCount {
		go rt.analyzeWithDynamicTrackerWorker(jobs, addToAppChildren, report, &wg, logger)
	}
	go func() {
		for _, app := range apps {
			jobs <- app
		}
		close(jobs)
	}()
	go func() {
		wg.Wait()
		close(results)
	}()
	for r := range results {
		if r.err != nil {
			return nil, r.err
		}
	}
	return appChildren, nil
}

// analyzeWithDynamicTrackerWorker reads Applications from jobs and processes them,
// reporting errors via report and adding discovered keys through addKey.
func (rt *DynamicTracker) analyzeWithDynamicTrackerWorker(
	jobs <-chan *v1alpha1.Application,
	addToAppChildren func(resourceInfo common.ResourceInfo),
	report func(error),
	wg *sync.WaitGroup,
	logger *log.Entry,
) {
	defer wg.Done()
	for app := range jobs {
		logger.Info("Processing application", "applicationName", app.GetName())
		// This now returns []common.ResourceInfo thanks to our pkg/ refactor
		directResources, err := rt.GetDirectResourcesFromStatus(app, logger)
		if err != nil {
			logger.WithError(err).Error("Error getting direct resources from status")
			report(err)
			continue
		}
		if len(rt.resourceMapperStore) == 0 {
			report(fmt.Errorf("no destination clusters synced; ensure Applications have valid .spec.destination and Argo CD has access"))
			continue
		}

		// Resolve destination server once per app
		server := app.Spec.Destination.Server
		if server == "" {
			var err error
			if app.Spec.Destination.Name == "" {
				err := fmt.Errorf("both destination server and name are empty")
				logger.WithError(err).Error("Destination missing")
				report(err)
				continue
			}
			server, err = getClusterServerByName(context.Background(), rt.db, app.Spec.Destination.Name)
			if err != nil {
				logger.WithError(err).Error("Error getting cluster by name")
				report(fmt.Errorf("error getting cluster: %w", err))
				continue
			}
		}
		// Check if any direct resource is missing in cache
		rt.cacheMu.RLock()
		syncRequired := false
		for _, resource := range *directResources {
			k := resourcegraph.GetResourceKey(resource.Group, resource.Kind)
			if _, exists := rt.sharedRelationsCache[k]; !exists {
				syncRequired = true
				break
			}
		}
		rt.cacheMu.RUnlock()

		if syncRequired {
			// Ensure only one worker per cluster performs the sync; others wait.
			clusterLock := rt.getClusterSyncLock(server)
			clusterLock.Lock()

			// Re-check under the cluster lock in case another worker already synced.
			rt.cacheMu.RLock()
			stillMissing := false
			missingResource := ""
			for _, resource := range *directResources {
				k := resourcegraph.GetResourceKey(resource.Group, resource.Kind)
				if _, exists := rt.sharedRelationsCache[k]; !exists {
					stillMissing = true
					missingResource = resource.String()
					break
				}
			}
			rt.cacheMu.RUnlock()

			if stillMissing {
				logger.WithFields(log.Fields{
					"cluster":         server,
					"missingResource": missingResource,
				}).Info("Syncing cache for missing resource")
				rt.ensureSyncedSharedCacheOnHost(context.Background(), server, logger)
				// if the direct resources are not in the cache, add them to the cache as left nodes
				rt.cacheMu.Lock()
				for _, resource := range *directResources {
					k := resourcegraph.GetResourceKey(resource.Group, resource.Kind)
					if _, exists := rt.sharedRelationsCache[k]; !exists {
						rt.sharedRelationsCache[k] = hashset.New()
					}
				}
				rt.cacheMu.Unlock()
			}
			clusterLock.Unlock()
		}
		relations := resourcegraph.GetResourceRelation(rt.sharedRelationsCache, *directResources)
		for _, resourceInfo := range relations {
			addToAppChildren(resourceInfo)
		}
		report(nil)
	}
}

// GetDirectResourcesFromStatus returns the direct resources from the application status
func (rt *DynamicTracker) GetDirectResourcesFromStatus(app *v1alpha1.Application, logger *log.Entry) (*[]common.ResourceInfo, error) {
	logger.Info("Getting direct resources from status")
	resources, err := argocd.GetResourcesFromStatus(app)
	if err != nil {
		return nil, fmt.Errorf("failed to get resources from status: %w", err)
	}
	missingResources, err := argocd.GetMissingResources(app)
	if err != nil {
		return nil, fmt.Errorf("failed to get missing resources: %w", err)
	}
	resources = append(resources, missingResources...)
	err = rt.syncResourceMapper(app, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to sync resource mapper: %w", err)
	}
	return &resources, nil
}

// getClusterSyncLock returns a per-cluster mutex, creating it if needed.
// It is safe to call concurrently.
func (rt *DynamicTracker) getClusterSyncLock(server string) *sync.Mutex {
	rt.cacheMu.Lock()
	defer rt.cacheMu.Unlock()

	if rt.syncLocks == nil {
		rt.syncLocks = make(map[string]*sync.Mutex)
	}
	if l, ok := rt.syncLocks[server]; ok {
		return l
	}
	mu := &sync.Mutex{}
	rt.syncLocks[server] = mu
	return mu
}

func (rt *DynamicTracker) syncResourceMapper(app *v1alpha1.Application, logger *log.Entry) error {
	var err error
	server := app.Spec.Destination.Server
	if server == "" {
		if app.Spec.Destination.Name == "" {
			return fmt.Errorf("both destination server and name are empty")
		}
		server, err = getClusterServerByName(context.Background(), rt.db, app.Spec.Destination.Name)
		if err != nil {
			return fmt.Errorf("error getting cluster: %w", err)
		}
	}
	cluster, err := rt.db.GetCluster(context.Background(), server)
	if err != nil {
		return fmt.Errorf("resolve destination cluster: %w", err)
	}
	restCfg, err := restConfigFromCluster(cluster, rt.kubeconfig, logger)
	if err != nil {
		return fmt.Errorf("failed to create rest config: %w", err)
	}
	if _, exists := rt.resourceMapperStore[server]; !exists {
		mapper, err := resourcegraph.NewResourceMapper(restCfg)
		if err != nil {
			return fmt.Errorf("failed to create ResourceMapper: %w", err)
		}
		// Start CRD informer so add/update events invoke addToResourceList
		go mapper.StartInformer()
		rt.resourceMapperStore[server] = mapper
	}
	return nil
}

// restConfigFromCluster creates a rest.Config from a cluster
func restConfigFromCluster(c *v1alpha1.Cluster, kubeconfigPath string, logger *log.Entry) (*rest.Config, error) {
	tls := rest.TLSClientConfig{
		Insecure:   c.Config.Insecure,
		ServerName: c.Config.ServerName,
		CertData:   c.Config.CertData,
		KeyData:    c.Config.KeyData,
		CAData:     c.Config.CAData,
	}

	var cfg *rest.Config

	// if the server is in-cluster, load the kubeconfig from the kubeconfig file or the default kubeconfig file
	if strings.Contains(c.Server, "kubernetes.default.svc") {
		var localCfg *rest.Config
		var err error
		if kubeconfigPath != "" {
			localCfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		} else {
			loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
			configOverrides := &clientcmd.ConfigOverrides{}
			kubeCfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
			localCfg, err = kubeCfg.ClientConfig()
		}
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
		}
		cfg = localCfg
	} else {

		switch {
		case c.Config.AWSAuthConfig != nil:
			// EKS via argocd-k8s-auth (same contract as Argo CD)
			args := []string{"aws", "--cluster-name", c.Config.AWSAuthConfig.ClusterName}
			if c.Config.AWSAuthConfig.RoleARN != "" {
				args = append(args, "--role-arn", c.Config.AWSAuthConfig.RoleARN)
			}
			if c.Config.AWSAuthConfig.Profile != "" {
				args = append(args, "--profile", c.Config.AWSAuthConfig.Profile)
			}
			cfg = &rest.Config{
				Host:            c.Server,
				TLSClientConfig: tls,
				ExecProvider: &clientauthapi.ExecConfig{
					APIVersion:      "client.authentication.k8s.io/v1beta1",
					Command:         "argocd-k8s-auth",
					Args:            args,
					InteractiveMode: clientauthapi.NeverExecInteractiveMode,
				},
			}

		case c.Config.ExecProviderConfig != nil:
			// Generic exec provider (OIDC, SSO, etc.)
			var env []clientauthapi.ExecEnvVar
			for k, v := range c.Config.ExecProviderConfig.Env {
				env = append(env, clientauthapi.ExecEnvVar{Name: k, Value: v})
			}
			cfg = &rest.Config{
				Host:            c.Server,
				TLSClientConfig: tls,
				ExecProvider: &clientauthapi.ExecConfig{
					APIVersion:      c.Config.ExecProviderConfig.APIVersion,
					Command:         c.Config.ExecProviderConfig.Command,
					Args:            c.Config.ExecProviderConfig.Args,
					Env:             env,
					InstallHint:     c.Config.ExecProviderConfig.InstallHint,
					InteractiveMode: clientauthapi.NeverExecInteractiveMode,
				},
			}

		default:
			// Static auth (token or basic) and TLS
			cfg = &rest.Config{
				Host:            c.Server,
				Username:        c.Config.Username,
				Password:        c.Config.Password,
				BearerToken:     c.Config.BearerToken,
				TLSClientConfig: tls,
			}
		}
	}

	if c.Config.ProxyUrl != "" {
		u, err := v1alpha1.ParseProxyUrl(c.Config.ProxyUrl)
		if err != nil {
			return nil, fmt.Errorf("failed to parse proxy url: %w", err)
		}
		cfg.Proxy = http.ProxyURL(u)
	}
	// Apply Argo CD defaults
	cfg.DisableCompression = c.Config.DisableCompression
	cfg.Timeout = v1alpha1.K8sServerSideTimeout
	cfg.QPS = v1alpha1.K8sClientConfigQPS
	cfg.Burst = v1alpha1.K8sClientConfigBurst
	v1alpha1.SetK8SConfigDefaults(cfg)

	return cfg, nil
}

func getClusterServerByName(ctx context.Context, db db.ArgoDB, clusterName string) (string, error) {
	servers, err := db.GetClusterServersByName(ctx, clusterName)
	if err != nil {
		return "", fmt.Errorf("error getting cluster server by name %q: %w", clusterName, err)
	}
	if len(servers) > 1 {
		return "", fmt.Errorf("there are %d clusters with the same name: %v", len(servers), servers)
	} else if len(servers) == 0 {
		return "", fmt.Errorf("there are no clusters with this name: %s", clusterName)
	}
	return servers[0], nil
}

// mergeInto adds rel (parent -> children) into dst
func mergeInto(dst, rel map[string]*hashset.Set) {
	for p, set := range rel {
		if _, ok := dst[p]; !ok {
			dst[p] = hashset.New()
		}
		for _, v := range set.Values() {
			if s, ok := v.(string); ok {
				dst[p].Add(s)
			}
		}
	}
}

// tracker.go
// ensureSyncedSharedCacheOnHost checks cache; if miss, queries ONLY the given server.
func (rt *DynamicTracker) ensureSyncedSharedCacheOnHost(ctx context.Context, server string, logger *log.Entry) {

	mapper, ok := rt.resourceMapperStore[server]
	if !ok || mapper == nil {
		// As a safety valve, you can return here OR (optionally) fall back to other mappers.
		logger.Warningf("No mapper for host %s", server)
		return
	}
	logger.Infof("Querying relations host=%s", server)
	rel, err := mapper.GetClusterResourcesRelation(ctx)
	if err != nil {
		logger.Warningf("Dynamic scan on %s failed: %v", server, err)
		return
	}
	rt.cacheMu.Lock()
	mergeInto(rt.sharedRelationsCache, rel)
	rt.cacheMu.Unlock()
}
