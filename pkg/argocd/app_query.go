package argocd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/argoproj/argo-cd/v2/pkg/apiclient"
	"github.com/argoproj/argo-cd/v2/pkg/apiclient/application"
	"github.com/argoproj/argo-cd/v2/pkg/apiclient/cluster"
	"github.com/argoproj/argo-cd/v2/pkg/apiclient/project"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type appClient struct {
	client apiclient.Client
}

type ClientOptions struct {
	ServerAddr      string
	Insecure        bool
	Plaintext       bool
	Certfile        string
	GRPCWeb         bool
	GRPCWebRootPath string
	AuthToken       string
}

func NewAPIClient(opts *ClientOptions) (appClient, error) {
	envAuthToken := os.Getenv("ARGOCD_TOKEN")
	if envAuthToken != "" && opts.AuthToken == "" {
		opts.AuthToken = envAuthToken
	}

	rOpts := &apiclient.ClientOptions{
		ServerAddr:      opts.ServerAddr,
		PlainText:       opts.Plaintext,
		Insecure:        opts.Insecure,
		CertFile:        opts.Certfile,
		GRPCWeb:         opts.GRPCWeb,
		GRPCWebRootPath: opts.GRPCWebRootPath,
		AuthToken:       opts.AuthToken,
	}
	client, err := apiclient.NewClient(rOpts)
	if err != nil {
		return appClient{}, fmt.Errorf("failed to create ArgoCD client: %w", err)
	}
	return appClient{client}, nil
}

func (a *appClient) GetApplicationChildManifests(ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject) ([]*unstructured.Unstructured, error) {
	query := &application.ApplicationManifestQuery{
		Name:         &app.Name,
		AppNamespace: &app.Namespace,
	}

	fmt.Printf("Querying manifests for app: %s, namespace: %s\n", app.Name, app.Namespace)
	conn, appSetClient, err := a.client.NewApplicationClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create Application client: %w", err)
	}
	defer conn.Close()
	response, err := appSetClient.GetManifests(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to get manifests: %w", err)
	}
	// print pretty the manifests for debugging
	for _, manifest := range response.Manifests {
		fmt.Printf("Manifest for app %s:\n%s\n", app.Name, manifest)
	}

	for i, manifest := range response.Manifests {
		var parsed interface{}
		err := yaml.Unmarshal([]byte(manifest), &parsed)
		if err != nil {
			fmt.Printf("Error parsing manifest %d: %v\n", i+1, err)
			continue
		}

		formatted, err := yaml.Marshal(parsed)
		if err != nil {
			fmt.Printf("Error formatting manifest %d: %v\n", i+1, err)
			continue
		}
		fmt.Printf("\n--- Manifest %d for app '%s' ---\n%s\n", i+1, app.Name, string(formatted))
	}
	return unmarshalManifests(response.Manifests)
}

func (a *appClient) GetApplication(ctx context.Context, appName, appNamespace string) (*v1alpha1.Application, error) {
	conn, appSetClient, err := a.client.NewApplicationClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create Application client: %w", err)
	}
	defer conn.Close()
	query := &application.ApplicationQuery{
		Name:         &appName,
		AppNamespace: &appNamespace,
	}

	return appSetClient.Get(ctx, query)
}

func (a *appClient) ListApplications(ctx context.Context, controllerNamespace string) ([]v1alpha1.Application, error) {
	var applications []v1alpha1.Application
	conn, appSetClient, err := a.client.NewApplicationClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create Application client: %w", err)
	}
	defer conn.Close()
	appList, err := appSetClient.List(ctx, &application.ApplicationQuery{})
	if err != nil {
		return nil, fmt.Errorf("failed to list applications: %w", err)
	}
	for _, app := range appList.Items {
		if app.Status.ControllerNamespace == controllerNamespace {
			applications = append(applications, app)
		}
	}
	return applications, nil
}

func (a *appClient) GetAppProject(ctx context.Context, name string) (*v1alpha1.AppProject, error) {
	conn, projectClient, err := a.client.NewProjectClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create Project client: %w", err)
	}
	defer conn.Close()

	return projectClient.Get(ctx, &project.ProjectQuery{Name: name})
}

func unmarshalManifests(manifests []string) ([]*unstructured.Unstructured, error) {
	targetObjs := make([]*unstructured.Unstructured, 0)
	for _, manifest := range manifests {
		if manifest == "" || manifest == "null" {
			return nil, nil
		}
		var obj unstructured.Unstructured
		err := json.Unmarshal([]byte(manifest), &obj)
		if err != nil {
			return nil, err
		}
		targetObjs = append(targetObjs, &obj)
	}
	return targetObjs, nil
}

func (a *appClient) GetApplicationDestinationServer(ctx context.Context, destinationServer, destinationName string) (*v1alpha1.Cluster, error) {
	conn, clusterClient, err := a.client.NewClusterClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create Application client: %w", err)
	}
	defer conn.Close()

	return clusterClient.Get(ctx, &cluster.ClusterQuery{
		Name:   destinationName,
		Server: destinationServer,
	})

}
