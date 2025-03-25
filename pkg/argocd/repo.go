package argocd

import (
	"context"
	"fmt"
	"log"

	"github.com/argoproj/argo-cd/v2/common"
	appsv1alpha1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v2/reposerver/apiclient"
	"github.com/argoproj/argo-cd/v2/util/argo"
	"github.com/argoproj/argo-cd/v2/util/db"
	"github.com/argoproj/argo-cd/v2/util/env"
	"github.com/argoproj/argo-cd/v2/util/io"
	kubeutil "github.com/argoproj/argo-cd/v2/util/kube"
	"github.com/argoproj/argo-cd/v2/util/settings"
	"github.com/argoproj/argo-cd/v2/util/tls"
	"github.com/argoproj/gitops-engine/pkg/utils/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type clusterAPIDetails struct {
	APIVersions  string
	APIResources []kube.APIResourceInfo
}

type repoServerManager struct {
	db            db.ArgoDB
	settingsMgr   *settings.SettingsManager
	repoClientset apiclient.Clientset
	kubectl       kube.Kubectl
}

func NewRepoServerManager(kubeClient kubernetes.Interface, controllerNamespace string, repoServerAddress string, repoServerTimeoutSeconds int, repoServerPlaintext bool, repoServerStrictTLS bool) *repoServerManager {
	settingsMgr := settings.NewSettingsManager(context.Background(), kubeClient, controllerNamespace)
	dbInstance := db.NewDB(controllerNamespace, settingsMgr, kubeClient)
	tlsConfig := apiclient.TLSConfiguration{
		DisableTLS:       repoServerPlaintext,
		StrictValidation: repoServerStrictTLS,
	}
	if !tlsConfig.DisableTLS && tlsConfig.StrictValidation {
		pool, err := tls.LoadX509CertPool(
			fmt.Sprintf("%s/reposerver/tls/tls.crt", env.StringFromEnv(common.EnvAppConfigPath, common.DefaultAppConfigPath)),
			fmt.Sprintf("%s/reposerver/tls/ca.crt", env.StringFromEnv(common.EnvAppConfigPath, common.DefaultAppConfigPath)),
		)
		if err != nil {
			log.Fatalf("Failed to load tls certs: %v", err)
		}
		tlsConfig.Certificates = pool
	}
	repoClientset := apiclient.NewRepoServerClientset(repoServerAddress, repoServerTimeoutSeconds, tlsConfig)
	kubectl := kubeutil.NewKubectl()

	return &repoServerManager{
		db:            dbInstance,
		settingsMgr:   settingsMgr,
		repoClientset: repoClientset,
		kubectl:       kubectl,
	}
}

// getApplicationChildManifests fetches manifests and filters direct child resources
func getApplicationChildManifests(ctx context.Context, application *appsv1alpha1.Application, proj *appsv1alpha1.AppProject, controllerNamespace string, repoServerManager *repoServerManager) ([]*unstructured.Unstructured, *rest.Config, error) {
	// Fetch Helm repositories
	helmRepos, err := repoServerManager.db.ListHelmRepositories(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("error fetching Helm repositories: %w", err)
	}
	// Filter permitted Helm repositories
	permittedHelmRepos, err := argo.GetPermittedRepos(proj, helmRepos)
	if err != nil {
		return nil, nil, fmt.Errorf("error filtering permitted Helm repositories: %w", err)
	}
	// Fetch Helm repository credentials
	helmRepositoryCredentials, err := repoServerManager.db.GetAllHelmRepositoryCredentials(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("error fetching Helm repository credentials: %w", err)
	}
	// Filter permitted Helm credentials
	permittedHelmCredentials, err := argo.GetPermittedReposCredentials(proj, helmRepositoryCredentials)
	if err != nil {
		return nil, nil, fmt.Errorf("error filtering permitted Helm credentials: %w", err)
	}
	// Get enabled source types
	enabledSourceTypes, err := repoServerManager.settingsMgr.GetEnabledSourceTypes()
	if err != nil {
		return nil, nil, fmt.Errorf("error getting enabled source types: %w", err)
	}
	// Fetch Helm settings
	helmOptions, err := repoServerManager.settingsMgr.GetHelmSettings()
	if err != nil {
		return nil, nil, fmt.Errorf("error fetching Helm settings: %w", err)
	}
	// Get installation ID
	installationID, err := repoServerManager.settingsMgr.GetInstallationID()
	if err != nil {
		return nil, nil, fmt.Errorf("error getting installation ID: %w", err)
	}
	kustomizeSettings, err := repoServerManager.settingsMgr.GetKustomizeSettings()
	if err != nil {
		return nil, nil, fmt.Errorf("error fetching Kustomize settings: %w", err)
	}
	server := application.Spec.Destination.Server
	if server == "" {
		if application.Spec.Destination.Name == "" {
			return nil, nil, fmt.Errorf("both destination server and name are empty")
		}
		server, err = getDestinationServer(ctx, repoServerManager.db, application.Spec.Destination.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("error getting cluster: %w", err)
		}
	}
	cluster, err := repoServerManager.db.GetCluster(ctx, server)
	if err != nil {
		return nil, nil, fmt.Errorf("error getting cluster: %w", err)
	}
	clusterAPIDetails, err := getClusterAPIDetails(cluster.RESTConfig(), repoServerManager.kubectl)
	if err != nil {
		return nil, nil, fmt.Errorf("error fetching cluster API details: %w", err)
	}
	// Establish a connection with the repo-server
	conn, repoClient, err := repoServerManager.repoClientset.NewRepoServerClient()
	if err != nil {
		return nil, nil, fmt.Errorf("error connecting to repo-server: %w", err)
	}
	defer io.Close(conn)
	sources := make([]appsv1alpha1.ApplicationSource, 0)
	revisions := make([]string, 0)
	if application.Spec.HasMultipleSources() {
		for _, source := range application.Spec.Sources {
			sources = append(sources, source)
			revisions = append(revisions, source.TargetRevision)
		}
	} else {
		revision := application.Spec.GetSource().TargetRevision
		revisions = append(revisions, revision)
		sources = append(sources, application.Spec.GetSource())
	}
	refSources, err := argo.GetRefSources(ctx, sources, application.Spec.Project, repoServerManager.db.GetRepository, revisions, false)
	if err != nil {
		return nil, nil, fmt.Errorf("error getting ref sources: %w", err)
	}
	targetObjs := make([]*unstructured.Unstructured, 0)
	for i, source := range sources {
		repo, err := repoServerManager.db.GetRepository(ctx, source.RepoURL, proj.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("error fetching repository: %w", err)
		}
		kustomizeOptions, err := kustomizeSettings.GetOptions(source)
		if err != nil {
			return nil, nil, fmt.Errorf("error getting ref sources: %w", err)
		}
		// Generate manifest using the RepoServer client
		manifestInfo, err := repoClient.GenerateManifest(ctx, &apiclient.ManifestRequest{
			Repo:               repo,
			Repos:              permittedHelmRepos,
			Revision:           revisions[i],
			AppName:            application.InstanceName(controllerNamespace),
			Namespace:          application.Spec.Destination.Namespace,
			ApplicationSource:  &source,
			KustomizeOptions:   kustomizeOptions,
			KubeVersion:        clusterAPIDetails.APIVersions,
			ApiVersions:        argo.APIResourcesToStrings(clusterAPIDetails.APIResources, true),
			HelmRepoCreds:      permittedHelmCredentials,
			TrackingMethod:     string(argo.GetTrackingMethod(repoServerManager.settingsMgr)),
			EnabledSourceTypes: enabledSourceTypes,
			HelmOptions:        helmOptions,
			HasMultipleSources: application.Spec.HasMultipleSources(),
			RefSources:         refSources,
			ProjectName:        proj.Name,
			ProjectSourceRepos: proj.Spec.SourceRepos,
			InstallationID:     installationID,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("error generating manifest: %w", err)
		}
		targetObj, err := unmarshalManifests(manifestInfo.Manifests)
		if err != nil {
			return nil, nil, fmt.Errorf("error unmarshalling manifests: %w", err)
		}
		targetObjs = append(targetObjs, targetObj...)
	}
	return targetObjs, cluster.RESTConfig(), nil
}

func unmarshalManifests(manifests []string) ([]*unstructured.Unstructured, error) {
	targetObjs := make([]*unstructured.Unstructured, 0)
	for _, manifest := range manifests {
		obj, err := appsv1alpha1.UnmarshalToUnstructured(manifest)
		if err != nil {
			return nil, err
		}
		targetObjs = append(targetObjs, obj)
	}
	return targetObjs, nil
}

// getClusterAPIDetails retrieves the server version and API resources from the Kubernetes cluster
func getClusterAPIDetails(config *rest.Config, kubectl kube.Kubectl) (*clusterAPIDetails, error) {
	// Retrieve the server version
	serverVersion, err := kubectl.GetServerVersion(config)
	if err != nil {
		return nil, fmt.Errorf("failed to get server version: %w", err)
	}
	// Retrieve the API resources
	apiResources, err := kubectl.GetAPIResources(config, false, &settings.ResourcesFilter{})
	if err != nil {
		return nil, fmt.Errorf("failed to get API resources: %w", err)
	}
	// Return the combined details
	return &clusterAPIDetails{
		APIVersions:  serverVersion,
		APIResources: apiResources,
	}, nil
}

// getDestinationServer retrieves the server version and API resources from the Kubernetes cluster
func getDestinationServer(ctx context.Context, db db.ArgoDB, clusterName string) (string, error) {
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
