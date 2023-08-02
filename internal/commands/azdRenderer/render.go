package azdRenderer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/azd"
	"github.com/microsoft/usvc-apiserver/pkg/dcpclient"
	"github.com/microsoft/usvc-apiserver/pkg/extensions"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
)

var (
	renderWorkloadFlags rendererData
)

func NewRenderWorkloadCommand() *cobra.Command {
	renderWorkloadCmd := &cobra.Command{
		Use:   "render-workload",
		Short: "Renders the workload of an application",
		RunE:  renderWorkload,
		Args:  cobra.NoArgs,
	}

	kubeconfig.EnsureKubeconfigFlag(renderWorkloadCmd.Flags())

	addRootDirFlag(renderWorkloadCmd, &renderWorkloadFlags.appRootDir)

	return renderWorkloadCmd
}

func renderWorkload(cmd *cobra.Command, _ []string) error {
	result, reason, err := canRender(renderWorkloadFlags.appRootDir)
	if err != nil {
		return err
	}
	if result == extensions.CanRenderResultNo {
		return fmt.Errorf(reason)
	}

	_, err = kubeconfig.EnsureKubeconfigFlagValue(cmd.Flags())
	if err != nil {
		return fmt.Errorf("cannot set up connection to the API server without kubeconfig file: %w", err)
	}

	client, err := dcpclient.New(cmd.Context(), 30*time.Second)
	if err != nil {
		return err
	}

	workload, err := createWorkload(cmd.Context(), client)
	if err != nil {
		return fmt.Errorf("Application run failed. An error occurred when creating the workload: %w", err)
	}

	for _, obj := range workload {
		err = client.Create(cmd.Context(), obj, &ctrl_client.CreateOptions{})
		if err != nil {
			// TODO: "roll back", i.e. delete, all objects that have been created up to this point

			return fmt.Errorf("Application run failed. An error occurred when creating object '%s' of type '%s': %w", obj.GetName(), obj.GetObjectKind().GroupVersionKind().Kind, err)
		}
	}

	return nil
}

func createWorkload(ctx context.Context, client ctrl_client.Client) ([]ctrl_client.Object, error) {
	azureYamlPath := filepath.Join(renderWorkloadFlags.appRootDir, "azure.yaml")
	yamlContent, err := os.ReadFile(azureYamlPath)
	if err != nil {
		return nil, fmt.Errorf("could not read azure.yaml file: %w", err)
	}

	projectConfig, err := azd.ParseProjectConfig(yamlContent)
	if err != nil {
		return nil, fmt.Errorf("could not parse azure.yaml file: %w", err)
	}

	objects := []ctrl_client.Object{}

	for _, svc := range projectConfig.Services {
		var o ctrl_client.Object

		switch {
		case (svc.Host == azd.HostTypeAppService || svc.Host == azd.HostTypeStaticWebApp) && svc.Language == azd.ProgrammingLanguageNodeJS:
			o, err = renderAppService(renderWorkloadFlags.appRootDir, svc)
		case svc.Host == azd.HostTypeFunction && svc.Language == azd.ProgrammingLanguageDotNet:
			o, err = renderFunction(renderWorkloadFlags.appRootDir, svc)
		case svc.Host == azd.HostTypeMsSQL:
			o, err = renderMsSQL(renderWorkloadFlags.appRootDir, svc)
		default:
			err = fmt.Errorf("combination of service host type '%s' and programming language '%s' is not currently supported by AZD workload renderer", svc.Host, svc.Language)
		}

		if err != nil {
			return nil, err
		} else {
			objects = append(objects, o)
		}
	}

	return objects, nil
}

func renderAppService(appRootDir string, svc *azd.ServiceConfig) (ctrl_client.Object, error) {
	exe := apiv1.ExecutableReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableReplicaSetSpec{
			Replicas: 1,
			Template: apiv1.ExecutableTemplate{
				Spec: apiv1.ExecutableSpec{
					ExecutablePath:   "npm",
					WorkingDirectory: filepath.Join(appRootDir, svc.RelativePath),
					Args:             []string{"run", "start"},
					Env: []apiv1.EnvVar{
						{Name: "BROWSER", Value: "none"},
					},
					EnvFiles: []string{filepath.Join(appRootDir, "local.env")},
				},
			},
		},
	}

	return &exe, nil
}

func renderFunction(appRootDir string, svc *azd.ServiceConfig) (ctrl_client.Object, error) {
	exe := apiv1.ExecutableReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableReplicaSetSpec{
			Replicas: 1,
			Template: apiv1.ExecutableTemplate{
				Spec: apiv1.ExecutableSpec{
					ExecutablePath:   "func",
					WorkingDirectory: filepath.Join(appRootDir, svc.RelativePath),
					Args:             []string{"host", "start", "--cors", "*"},
					EnvFiles:         []string{filepath.Join(appRootDir, "local.env")},
				},
			},
		},
	}

	return &exe, nil
}

func renderMsSQL(appRootDir string, svc *azd.ServiceConfig) (ctrl_client.Object, error) {
	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: "mcr.microsoft.com/mssql/server:2022-latest",
			VolumeMounts: []apiv1.VolumeMount{
				{
					Type:   apiv1.NamedVolumeMount,
					Source: "sql-data",
					Target: "/data/db",
				},
			},
			Ports: []apiv1.ContainerPort{
				{HostPort: 1433, ContainerPort: 1433},
			},
			Env: []apiv1.EnvVar{
				{Name: "ACCEPT_EULA", Value: "Y"},
			},
			EnvFiles:      []string{filepath.Join(appRootDir, "local.env")},
			RestartPolicy: apiv1.RestartPolicyUnlessStopped,
		},
	}

	return &ctr, nil
}
