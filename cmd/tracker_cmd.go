package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/anandf/resource-tracker/pkg/analyzer"
	dynamicbackend "github.com/anandf/resource-tracker/pkg/analyzer/dynamic"
	graphbackend "github.com/anandf/resource-tracker/pkg/analyzer/graph"
	"github.com/anandf/resource-tracker/pkg/common"
	"github.com/anandf/resource-tracker/pkg/env"
	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/anandf/resource-tracker/pkg/version"
	argocdcommon "github.com/argoproj/argo-cd/v3/common"
	kubeutil "github.com/argoproj/argo-cd/v3/util/kube"
	"github.com/avitaltamir/cyphernetes/pkg/core"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type queryCLIConfig struct {
	applicationName          string
	applicationNamespace     string
	logLevel                 string
	kubeConfig               string
	repoServerAddress        string
	repoServerPlaintext      bool
	repoServerStrictTLS      bool
	repoServerTimeoutSeconds int
	argocdNamespace          string
	strategy                 string // 'dynamic' or 'graph'
	allApps                  bool
}

// NewAnalyzeCommand creates the 'analyze' command, which is the primary entrypoint.
func NewAnalyzeCommand() *cobra.Command {
	cfg := &queryCLIConfig{}

	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Analyze resource relationships and dependencies for ArgoCD applications",
		Long:  "Analyze resource relationships and dependencies for ArgoCD applications. Can process a single app or all apps.",
		RunE: func(cmd *cobra.Command, args []string) error {
			log.Infof("%s %s starting [loglevel:%s]",
				version.BinaryName(),
				version.Version(),
				strings.ToUpper(cfg.logLevel),
			)
			level, err := log.ParseLevel(cfg.logLevel)
			if err != nil {
				return fmt.Errorf("failed to parse log level: %w", err)
			}
			log.SetLevel(level)
			core.LogLevel = cfg.logLevel

			// Require --app when --all-apps is false, to avoid silently analyzing all apps.
			if !cfg.allApps && cfg.applicationName == "" {
				return fmt.Errorf("application name is required to analyze a single application")
			}

			// Support app specified as "namespace/name" similar to `argocd app get ns/name`.
			// If provided, override the separate applicationNamespace flag.
			if cfg.applicationName != "" && !cfg.allApps {
				if parts := strings.SplitN(cfg.applicationName, "/", 2); len(parts) == 2 {
					cfg.applicationNamespace = parts[0]
					cfg.applicationName = parts[1]
					log.WithFields(log.Fields{
						"applicationName":      cfg.applicationName,
						"applicationNamespace": cfg.applicationNamespace,
					}).Debug("Parsed application from namespace/name syntax")
				}
			}

			// Load the Kubernetes REST config (in-cluster or from kubeconfig path).
			restCfg, err := kube.GetKubeConfig(cfg.kubeConfig)
			if err != nil {
				return fmt.Errorf("failed to load kubeconfig: %w", err)
			}
			repoAddr, err := ensureRepoServerAddress(restCfg, cfg.argocdNamespace, cfg.repoServerAddress)
			if err != nil {
				return err
			}

			opts := analyzer.Options{
				KubeConfig:               restCfg,
				KubeConfigPath:           cfg.kubeConfig,
				ArgoCDNamespace:          cfg.argocdNamespace,
				TargetApp:                cfg.applicationName,
				TargetAppNamespace:       cfg.applicationNamespace,
				RepoServerAddress:        repoAddr,
				RepoServerPlaintext:      cfg.repoServerPlaintext,
				RepoServerStrictTLS:      cfg.repoServerStrictTLS,
				RepoServerTimeoutSeconds: cfg.repoServerTimeoutSeconds,
			}
			// Select backend.
			var backend analyzer.Backend
			switch cfg.strategy {
			case "graph":
				backend = graphbackend.NewBackend()
			case "dynamic":
				backend = dynamicbackend.NewBackend()
			default:
				return fmt.Errorf("invalid strategy: %s (must be 'graph' or 'dynamic')", cfg.strategy)
			}

			// Execute analysis.
			groupedKinds, err := backend.Execute(context.Background(), opts)
			if err != nil {
				return err
			}
			printInclusions(groupedKinds)
			return nil
		},
	}
	cmd.Flags().StringVar(&cfg.logLevel, "loglevel", env.GetStringVal("RESOURCE_TRACKER_LOGLEVEL", "info"), "set the loglevel to one of trace|debug|info|warn|error")
	cmd.Flags().StringVarP(&cfg.applicationName, "app", "", "", "Application name (required for single app analysis). Supports 'namespace/name' syntax.")
	cmd.Flags().StringVarP(&cfg.applicationNamespace, "app-namespace", "N", "argocd", "Application namespace")
	cmd.Flags().StringVar(&cfg.repoServerAddress, "repo-server", env.GetStringVal("ARGOCD_REPO_SERVER", ""), "Repo server address. If empty, the CLI will port-forward to the repo-server service.")
	cmd.Flags().BoolVar(&cfg.repoServerPlaintext, "repo-server-plaintext", false, "Use an unencrypted HTTP connection to the ArgoCD API instead of TLS.")
	cmd.Flags().BoolVar(&cfg.repoServerStrictTLS, "repo-server-strict-tls", false, "Enable strict TLS validation for the repo server connection.")
	cmd.Flags().IntVar(&cfg.repoServerTimeoutSeconds, "repo-server-timeout-seconds", 60, "Timeout in seconds for repo server RPC calls.")
	cmd.Flags().StringVarP(&cfg.argocdNamespace, "namespace", "n", "argocd", "ArgoCD namespace")
	cmd.Flags().StringVar(&cfg.kubeConfig, "kubeconfig", "", "Path to kubeconfig file for cluster access")
	cmd.Flags().BoolVar(&cfg.allApps, "all-apps", false, "Analyze all applications in the namespace")
	cmd.Flags().StringVar(&cfg.strategy, "strategy", "graph", "Analysis strategy: 'dynamic' (OwnerRef walking) or 'graph' (Cyphernetes)")
	return cmd
}

func printInclusions(groupedKinds *common.GroupedResourceKinds) {
	resourceInclusionString := groupedKinds.String()
	if strings.HasPrefix(resourceInclusionString, "error:") {
		log.Errorf("error generating resource.inclusions: %s", resourceInclusionString)
		return
	}
	log.Infof("resource.inclusions: |\n%s", resourceInclusionString)
}

func ensureRepoServerAddress(restCfg *rest.Config, namespace, current string) (string, error) {
	if current != "" {
		return current, nil
	}
	log.Infof("Repo server address not provided, attempting to port-forward to argocd-repo-server in namespace %q", namespace)

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return "", fmt.Errorf("failed to create kube client for port-forward: %w", err)
	}
	// Discover the repo-server service to determine the app name label.
	svcSelector := fmt.Sprintf("%s=%s", argocdcommon.LabelKeyComponentRepoServer, argocdcommon.LabelValueComponentRepoServer)
	services, err := clientset.CoreV1().Services(namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: svcSelector,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list repo-server services: %w", err)
	}
	repoServerName := "argocd-repo-server"
	if len(services.Items) > 0 {
		if v, ok := services.Items[0].Labels[argocdcommon.LabelKeyAppName]; ok && v != "" {
			repoServerName = v
		}
	}
	// Use the same label selector Argo CD uses for the repo-server pods.
	podSelector := fmt.Sprintf("%s=%s", argocdcommon.LabelKeyAppName, repoServerName)
	overrides := clientcmd.ConfigOverrides{}

	localPort, err := kubeutil.PortForward(8081, namespace, &overrides, podSelector)
	if err != nil {
		return "", fmt.Errorf("failed to port-forward to repo-server: %w", err)
	}

	addr := fmt.Sprintf("localhost:%d", localPort)
	log.WithField("repoServerAddress", addr).Info("Using port-forwarded repo-server address")
	return addr, nil
}
