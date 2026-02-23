package repo

import (
	"context"
	"fmt"

	"github.com/argoproj/argo-cd/v3/common"
	appsv1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/reposerver/apiclient"
	"github.com/argoproj/argo-cd/v3/util/argo"
	"github.com/argoproj/argo-cd/v3/util/db"
	"github.com/argoproj/argo-cd/v3/util/env"
	"github.com/argoproj/argo-cd/v3/util/io"
	"github.com/argoproj/argo-cd/v3/util/settings"
	"github.com/argoproj/argo-cd/v3/util/tls"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type RepoServerManager struct {
	db            db.ArgoDB
	settingsMgr   *settings.SettingsManager
	repoClientset apiclient.Clientset
}

func NewRepoServerManager(
	kubeConfig *rest.Config,
	controllerNamespace string,
	repoServerAddress string,
	repoServerTimeoutSeconds int,
	repoServerPlaintext bool,
	repoServerStrictTLS bool,
) (*RepoServerManager, error) {
	clientSet, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return nil, err
	}
	settingsMgr := settings.NewSettingsManager(context.Background(), clientSet, controllerNamespace)
	dbInstance := db.NewDB(controllerNamespace, settingsMgr, clientSet)
	tlsConfig := apiclient.TLSConfiguration{
		DisableTLS:       repoServerPlaintext,
		StrictValidation: repoServerStrictTLS,
	}
	if !tlsConfig.DisableTLS && tlsConfig.StrictValidation {
		appConfigPath := env.StringFromEnv(common.EnvAppConfigPath, common.DefaultAppConfigPath)
		pool, err := tls.LoadX509CertPool(
			appConfigPath+"/reposerver/tls/tls.crt",
			appConfigPath+"/reposerver/tls/ca.crt",
		)
		if err != nil {
			return nil, fmt.Errorf("failed to load tls certs: %w", err)
		}
		tlsConfig.Certificates = pool
	}
	repoClientset := apiclient.NewRepoServerClientset(repoServerAddress, repoServerTimeoutSeconds, tlsConfig)
	return &RepoServerManager{
		db:            dbInstance,
		settingsMgr:   settingsMgr,
		repoClientset: repoClientset,
	}, nil
}

// GetApplicationChildManifests fetches manifests and filters direct child resources
func (r *RepoServerManager) GetApplicationChildManifests(ctx context.Context, application *appsv1alpha1.Application, proj *appsv1alpha1.AppProject) ([]*unstructured.Unstructured, error) {
	// Fetch Helm repositories
	helmRepos, err := r.db.ListHelmRepositories(ctx)
	if err != nil {
		return nil, fmt.Errorf("error fetching Helm repositories: %w", err)
	}
	// Filter permitted Helm repositories
	permittedHelmRepos, err := argo.GetPermittedRepos(proj, helmRepos)
	if err != nil {
		return nil, fmt.Errorf("error filtering permitted Helm repositories: %w", err)
	}
	// Fetch Helm repository credentials
	helmRepositoryCredentials, err := r.db.GetAllHelmRepositoryCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("error fetching Helm repository credentials: %w", err)
	}
	// Filter permitted Helm credentials
	permittedHelmCredentials, err := argo.GetPermittedReposCredentials(proj, helmRepositoryCredentials)
	if err != nil {
		return nil, fmt.Errorf("error filtering permitted Helm credentials: %w", err)
	}
	// Get enabled source types
	enabledSourceTypes, err := r.settingsMgr.GetEnabledSourceTypes()
	if err != nil {
		return nil, fmt.Errorf("error getting enabled source types: %w", err)
	}
	kustomizeSettings, err := r.settingsMgr.GetKustomizeSettings()
	if err != nil {
		return nil, fmt.Errorf("error fetching Kustomize settings: %w", err)
	}
	// Establish a connection with the repo-server
	conn, repoClient, err := r.repoClientset.NewRepoServerClient()
	if err != nil {
		return nil, fmt.Errorf("error connecting to repo-server: %w", err)
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
	refSources, err := argo.GetRefSources(ctx, sources, application.Spec.Project, r.db.GetRepository, revisions, false)
	if err != nil {
		return nil, fmt.Errorf("error getting ref sources: %w", err)
	}
	// Resolve destination cluster to get the real kube version
	var kubeVersion string
	var apiVersions []string
	dest := application.Spec.Destination
	if dest.Server != "" || dest.Name != "" {
		server := dest.Server
		if server == "" && dest.Name != "" {
			servers, err := r.db.GetClusterServersByName(ctx, dest.Name)
			if err == nil && len(servers) > 0 {
				server = servers[0]
			}
		}
		if server != "" {
			cluster, err := r.db.GetCluster(ctx, server)
			if err == nil {
				kubeVersion = cluster.Info.ServerVersion
				apiVersions = cluster.Info.APIVersions
			}
		}
	}
	targetObjs := make([]*unstructured.Unstructured, 0)
	for i, source := range sources {
		repo, err := r.db.GetRepository(ctx, source.RepoURL, proj.Name)
		if err != nil {
			return nil, fmt.Errorf("error fetching repository: %w", err)
		}
		kustomizeOptions, err := kustomizeSettings.GetOptions(source)
		if err != nil {
			return nil, fmt.Errorf("error getting ref sources: %w", err)
		}
		// Generate manifest using the RepoServer client
		manifestInfo, err := repoClient.GenerateManifest(ctx, &apiclient.ManifestRequest{
			Repo:               repo,
			Repos:              permittedHelmRepos,
			Revision:           revisions[i],
			ApplicationSource:  &source,
			EnabledSourceTypes: enabledSourceTypes,
			KustomizeOptions:   kustomizeOptions,
			KubeVersion:        kubeVersion,
			ApiVersions:        apiVersions,
			HelmRepoCreds:      permittedHelmCredentials,
			HasMultipleSources: application.Spec.HasMultipleSources(),
			RefSources:         refSources,
		})
		if err != nil {
			return nil, fmt.Errorf("error generating manifest: %w", err)
		}
		targetObj, err := unmarshalManifests(manifestInfo.Manifests)
		if err != nil {
			return nil, fmt.Errorf("error unmarshalling manifests: %w", err)
		}
		targetObjs = append(targetObjs, targetObj...)
		log.Debugf("Successfully fetched %v target manifest(s) from repo-server for application: %s. Manifests: %v", len(manifestInfo.Manifests), application.Name, manifestInfo.Manifests)
	}
	return targetObjs, nil
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
