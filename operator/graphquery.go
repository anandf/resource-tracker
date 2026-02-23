package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/avitaltamir/cyphernetes/pkg/core"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/anandf/resource-tracker/pkg/common"
	"github.com/anandf/resource-tracker/pkg/env"
	"github.com/anandf/resource-tracker/pkg/version"
)

type GraphQueryControllerConfig struct {
	BaseControllerConfig
}

type GraphQueryController struct {
	*BaseController
	cfg *GraphQueryControllerConfig
}

// newGraphQueryCommand implements "runQuery" command which executes a cyphernetes graph query against a given kubeconfig
func newGraphQueryCommand() *cobra.Command {
	cfg := &GraphQueryControllerConfig{}
	runQueryCmd := &cobra.Command{
		Use:   "run-query",
		Short: "Runs the resource-tracker which executes a graph based query to fetch the dependencies",
		RunE: func(_ *cobra.Command, _ []string) error {
			log.Infof("%s %s starting [loglevel:%s, interval:%s]",
				fmt.Sprintf("%s-%s", version.BinaryName(), "operator"),
				version.Version(),
				strings.ToUpper(cfg.logLevel),
				cfg.checkInterval,
			)
			var err error
			level, err := log.ParseLevel(cfg.logLevel)
			if err != nil {
				return fmt.Errorf("failed to parse log level: %w", err)
			}
			log.SetLevel(level)
			core.LogLevel = cfg.logLevel
			controller, err := newGraphQueryController(cfg)
			if err != nil {
				return err
			}
			return initApplicationInformer(controller.dynamicClient, controller)
		},
	}
	runQueryCmd.Flags().StringVar(&cfg.logLevel, "loglevel", env.GetStringVal("RESOURCE_TRACKER_LOGLEVEL", "info"), "set the loglevel to one of trace|debug|info|warn|error")
	runQueryCmd.Flags().StringVar(&cfg.kubeConfig, "kubeconfig", "", "full path to kube client configuration, i.e. ~/.kube/config")
	runQueryCmd.Flags().StringVar(&cfg.argocdNamespace, "argocd-namespace", "argocd", "namespace where argocd control plane components are running")
	cfg.updateEnabled = runQueryCmd.Flags().Bool("update-enabled", false, "if enabled updates the argocd-cm directly, else prints the output on screen")
	runQueryCmd.Flags().StringVar(&cfg.updateResourceName, "update-resource-name", "argocd-cm", "name of the resource that needs to be updated. Default: argocd-cm")
	runQueryCmd.Flags().StringVar(&cfg.updateResourceKind, "update-resource-kind", "ConfigMap", "kind of resource that needs to be updated, "+
		"users can choose to update either spec.data in argocd-cm or spec.extraConfigs in ArgoCD resource, Default: ConfigMap")
	runQueryCmd.Flags().DurationVar(&cfg.checkInterval, "interval", DefaultCheckInterval, "interval for how often to check for updates, "+
		"to avoid frequent execution of compute and memory intensive graph queries")
	return runQueryCmd
}

func newGraphQueryController(cfg *GraphQueryControllerConfig) (*GraphQueryController, error) {
	base, err := newBaseController(&cfg.BaseControllerConfig)
	if err != nil {
		return nil, err
	}
	return &GraphQueryController{
		BaseController: base,
		cfg:            cfg,
	}, nil
}

// execute runs the graph query, computes the resources managed via Argo CD and update the resource.inclusions
// settings in the argocd-cm config map if it detects any new changes compared to the previous computed value or if its
// value is different from what is present in the argocd-cm config map.
// if the check interval time has not passed since the previous run, then the method returns without executing any queries.
func (g *GraphQueryController) execute() error {
	if !g.lastRunTime.IsZero() && time.Since(g.lastRunTime) < g.cfg.checkInterval {
		log.Info("skipping query executor due to last run not lapsed the check interval")
		return nil
	}
	var allAppChildren []*common.ResourceInfo
	g.lastRunTime = time.Now()
	for host, qs := range g.queryServers {
		log.Infof("Querying Argo CD application globally for application in host %s", host)
		qs.VisitedKinds = make(map[common.ResourceInfo]bool)
		appChildren, err := qs.GetApplicationChildResources("", "")
		if err != nil {
			return err
		}
		log.Infof("Children of Argo CD application globally for application: %v", appChildren)
		for appChild := range appChildren {
			allAppChildren = append(allAppChildren, &appChild)
		}
	}

	groupedKinds := make(common.GroupedResourceKinds)
	groupedKinds.MergeResourceInfos(allAppChildren)
	missingResources, err := g.argoCDClient.GetAllMissingResources()
	if err != nil {
		return err
	}
	// Check if additional resources are missing, if so add it.
	for _, resource := range missingResources {
		log.Infof("adding missing resource '%v'", resource)
		if kindMap, ok := groupedKinds[resource.Group]; !ok && kindMap == nil {
			groupedKinds[resource.Group] = common.Kinds{resource.Kind: common.Void{}}
		} else {
			groupedKinds[resource.Group][resource.Kind] = common.Void{}
		}
	}
	if !*g.cfg.updateEnabled {
		if !g.previousGroupedKinds.Equal(&groupedKinds) {
			log.Info("direct update or argocd-cm is disabled, printing the output on terminal")
			resourceInclusionString := groupedKinds.String()
			if strings.HasPrefix(resourceInclusionString, "error:") {
				return fmt.Errorf("error in yaml string of resource.inclusions: %s", resourceInclusionString)
			}
			fmt.Printf("resource.inclusions: |\n%sresource.exclusions: ''\n", resourceInclusionString)
		} else {
			log.Infof("no changes detected in previously computed resource inclusions and current computed resource inclusions")
		}
	} else {
		if g.cfg.updateResourceKind == ArgoCDResourceKind {
			err = handleUpdateInArgoCDCR(g.argoCDClient, g.cfg.updateResourceName, g.cfg.argocdNamespace, groupedKinds)
			if err != nil {
				return err
			}
		} else {
			err = handleUpdateInCM(g.argoCDClient, g.cfg.argocdNamespace, groupedKinds)
			if err != nil {
				return err
			}
		}
	}
	g.previousGroupedKinds = groupedKinds
	return nil
}
