package main

import (
	"errors"
	"fmt"

	"github.com/anandf/resource-tracker/pkg/argocd" // <-- IMPORT COMMON
	"github.com/anandf/resource-tracker/pkg/common"
	"github.com/anandf/resource-tracker/pkg/graph"
	"github.com/avitaltamir/cyphernetes/pkg/core"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// GraphBackend implements the analysis using the cyphernetes graph query engine.
type GraphBackend struct {
	queryServer  *graph.QueryServer
	argoCDClient argocd.ArgoCD
}

// NewGraphBackend initializes the required kubernetes clients and the cyphernetes graph query executor.
func NewGraphBackend(cfg *QueryConfig) (*GraphBackend, error) {
	// First try in-cluster config
	restConfig, err := rest.InClusterConfig()
	if err != nil && !errors.Is(err, rest.ErrNotInCluster) {
		return nil, fmt.Errorf("failed to create config: %v", err)
	}

	// If not running in-cluster, try loading from KUBECONFIG env or $HOME/.kube/config file
	if restConfig == nil {
		// Fall back to kubeconfig
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		loadingRules.ExplicitPath = cfg.kubeConfig
		configOverrides := &clientcmd.ConfigOverrides{}
		kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
		restConfig, err = kubeConfig.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to create config: %v", err)
		}
	}

	argoCD, err := argocd.NewArgoCD(restConfig, cfg.argocdNamespace, cfg.applicationNamespace)
	if err != nil {
		return nil, err
	}
	trackingMethod, err := argoCD.GetTrackingMethod()
	if err != nil {
		return nil, err
	}
	// Set log level for cyphernetes core
	core.LogLevel = cfg.logLevel

	qs, err := graph.NewQueryServer(restConfig, trackingMethod, true)
	if err != nil {
		return nil, err
	}
	return &GraphBackend{
		queryServer:  qs,
		argoCDClient: argoCD,
	}, nil
}

// Execute executes the graph query and returns the computed kinds and an error.
func (b *GraphBackend) Execute(cfg *QueryConfig) (*common.GroupedResourceKinds, error) {
	logger := log.WithFields(log.Fields{
		"controllerNamespace":  cfg.argocdNamespace,
		"strategy":             "cyphernetes",
		"applicationName":      cfg.applicationName,
		"allApps":              cfg.allApps,
		"applicationNamespace": cfg.applicationNamespace,
	})
	logger.Info("Starting query executor...")
	var allAppChildrenGraph []common.ResourceInfo
	if *cfg.globalQuery {
		logger.Infof("Querying Argo CD application globally for application '%s'...", cfg.applicationName)
		appChildren, err := b.queryServer.GetApplicationChildResources(cfg.applicationName, "")
		if err != nil {
			return nil, err
		}
		logger.Debugf("Children of Argo CD application globally for application '%s': %s", cfg.applicationName, appChildren.String())
		for appChild := range appChildren {
			allAppChildrenGraph = append(allAppChildrenGraph, appChild)
		}
	} else {
		argoAppResources, err := b.argoCDClient.ListApplications()
		if err != nil {
			return nil, err
		}
		logger.Infof("Found %d applications", len(argoAppResources))
		for _, argoAppResource := range argoAppResources {
			logger.Debugf("Querying Argo CD application '%v'", argoAppResource)
			appChildren, err := b.queryServer.GetApplicationChildResources(argoAppResource.Name, "")
			if err != nil {
				logger.WithError(err).Error("Error getting application children")
				continue
			}
			logger.Debugf("Children of Argo CD application '%s': %v", argoAppResource.Name, appChildren)
			for appChild := range appChildren {
				allAppChildrenGraph = append(allAppChildrenGraph, appChild)
			}
		}
	}
	groupedKinds := make(common.GroupedResourceKinds)
	groupedKinds.MergeResourceInfos(allAppChildrenGraph)
	missingResources, err := b.argoCDClient.GetAllMissingResources()
	if err != nil {
		return nil, err
	}
	logger.Debugf("Found %d missing resources from app conditions", len(missingResources))
	groupedKinds.MergeResourceInfos(missingResources)
	return &groupedKinds, nil
}
