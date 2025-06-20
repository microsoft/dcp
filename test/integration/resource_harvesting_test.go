package integration_test

import (
	"fmt"
	"testing"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil/ctrlutil"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
	"github.com/stretchr/testify/require"
)

func TestUnusedNetworkHarvesting(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	log := testutil.NewLogForTesting(t.Name())
	co, coErr := ctrl_testutil.NewTestContainerOrchestrator(ctx, log, ctrl_testutil.TcoOptionNone)
	require.NoError(t, coErr, "could not create a test container orchestrator")
	defer require.NoError(t, co.Close())

	procNonExistent := nonExistentProcess(t)

	const prefix = "unused-network-harvesting-"

	// Network with no DCP labels (should be preserved)
	const netNoLabels = prefix + "no-labels"
	_, netCreateErr := co.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: netNoLabels})
	require.NoError(t, netCreateErr)

	// Persistent DCP network with no containers (should not be preserved)
	const netPersistent = prefix + "persistent"
	_, netCreateErr = co.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: netPersistent,
		Labels: map[string]string{
			controllers.PersistentLabel:              "true",
			controllers.CreatorProcessIdLabel:        fmt.Sprintf("%d", procNonExistent.Pid),
			controllers.CreatorProcessStartTimeLabel: procNonExistent.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
		},
	})
	require.NoError(t, netCreateErr)

	// Persistent DCP network with non-DCP containers (should be preserved)
	const netPersistentWithContainer = prefix + "persistent-with-container"
	_, netCreateErr = co.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: netPersistentWithContainer,
		Labels: map[string]string{
			controllers.PersistentLabel:              "true",
			controllers.CreatorProcessIdLabel:        fmt.Sprintf("%d", procNonExistent.Pid),
			controllers.CreatorProcessStartTimeLabel: procNonExistent.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
		},
	})
	require.NoError(t, netCreateErr)

	_, containerCreateErr := co.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name:    prefix + "non-dcp-container-1",
			Network: netPersistentWithContainer,
		},
	})
	require.NoError(t, containerCreateErr)

	procThis, procThisErr := process.This()
	require.NoError(t, procThisErr)

	// Network that is used by existing process (should be preserved)
	const netUsedByExistingProcess = prefix + "used-by-existing-process"
	_, netCreateErr = co.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: netUsedByExistingProcess,
		Labels: map[string]string{
			controllers.PersistentLabel:              "false",
			controllers.CreatorProcessIdLabel:        fmt.Sprintf("%d", procThis.Pid),
			controllers.CreatorProcessStartTimeLabel: procThis.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
		},
	})
	require.NoError(t, netCreateErr)

	// Network that has non-DCP containers attached to it (should be preserved)
	const netWithNonDcpContainers = prefix + "with-non-dcp-containers"
	_, netCreateErr = co.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: netWithNonDcpContainers,
		Labels: map[string]string{
			controllers.PersistentLabel:              "false",
			controllers.CreatorProcessIdLabel:        fmt.Sprintf("%d", procNonExistent.Pid),
			controllers.CreatorProcessStartTimeLabel: procNonExistent.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
		},
	})
	require.NoError(t, netCreateErr)
	_, containerCreateErr = co.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name:    prefix + "non-dcp-container-2",
			Network: netWithNonDcpContainers,
		},
	})
	require.NoError(t, containerCreateErr)
	_, containerCreateErr = co.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name:    prefix + "non-dcp-container-3",
			Network: netWithNonDcpContainers,
		},
	})
	require.NoError(t, containerCreateErr)

	// Network that has some non-DCP and some DCP abandoned containers attached to it (should be preserved)
	const netWithMixedContainers = prefix + "with-mixed-containers"
	_, netCreateErr = co.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: netWithMixedContainers,
		Labels: map[string]string{
			controllers.PersistentLabel:              "false",
			controllers.CreatorProcessIdLabel:        fmt.Sprintf("%d", procNonExistent.Pid),
			controllers.CreatorProcessStartTimeLabel: procNonExistent.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
		},
	})
	require.NoError(t, netCreateErr)
	_, containerCreateErr = co.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name:    prefix + "dcp-container-1",
			Network: netWithMixedContainers,
			ContainerSpec: apiv1.ContainerSpec{
				Labels: []apiv1.ContainerLabel{
					{
						Key:   controllers.PersistentLabel,
						Value: "false",
					},
					{
						Key:   controllers.CreatorProcessIdLabel,
						Value: fmt.Sprintf("%d", procNonExistent.Pid),
					},
					{
						Key:   controllers.CreatorProcessStartTimeLabel,
						Value: procNonExistent.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
					},
				},
			},
		},
	})
	require.NoError(t, containerCreateErr)
	_, containerCreateErr = co.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name:    prefix + "non-dcp-container-4",
			Network: netWithMixedContainers,
		},
	})
	require.NoError(t, containerCreateErr)

	// Abandoned network with abandoned, but persistent container (should be preserved)
	const netWithAbandonedPersistentContainer = prefix + "with-abandoned-persistent-container"
	_, netCreateErr = co.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: netWithAbandonedPersistentContainer,
		Labels: map[string]string{
			controllers.PersistentLabel:              "false",
			controllers.CreatorProcessIdLabel:        fmt.Sprintf("%d", procNonExistent.Pid),
			controllers.CreatorProcessStartTimeLabel: procNonExistent.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
		},
	})
	require.NoError(t, netCreateErr)
	_, containerCreateErr = co.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name:    prefix + "abandoned-persistent-container",
			Network: netWithAbandonedPersistentContainer,
			ContainerSpec: apiv1.ContainerSpec{
				Labels: []apiv1.ContainerLabel{
					{
						Key:   controllers.PersistentLabel,
						Value: "true",
					},
					{
						Key:   controllers.CreatorProcessIdLabel,
						Value: fmt.Sprintf("%d", procNonExistent.Pid),
					},
					{
						Key:   controllers.CreatorProcessStartTimeLabel,
						Value: procNonExistent.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
					},
				},
			},
		},
	})
	require.NoError(t, containerCreateErr)

	// Network that has only DCP abandoned, non-persistent containers attached to it (should be removed)
	const netWithDcpContainers = prefix + "with-dcp-containers"
	_, netCreateErr = co.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: netWithDcpContainers,
		Labels: map[string]string{
			controllers.PersistentLabel:              "false",
			controllers.CreatorProcessIdLabel:        fmt.Sprintf("%d", procNonExistent.Pid),
			controllers.CreatorProcessStartTimeLabel: procNonExistent.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
		},
	})
	require.NoError(t, netCreateErr)
	_, containerCreateErr = co.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name:    prefix + "dcp-container-2",
			Network: netWithDcpContainers,
			ContainerSpec: apiv1.ContainerSpec{
				Labels: []apiv1.ContainerLabel{
					{
						Key:   controllers.PersistentLabel,
						Value: "false",
					},
					{
						Key:   controllers.CreatorProcessIdLabel,
						Value: fmt.Sprintf("%d", procNonExistent.Pid),
					},
					{
						Key:   controllers.CreatorProcessStartTimeLabel,
						Value: procNonExistent.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
					},
				},
			},
		},
	})
	require.NoError(t, containerCreateErr)
	_, containerCreateErr = co.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name:    prefix + "dcp-container-3",
			Network: netWithDcpContainers,
			ContainerSpec: apiv1.ContainerSpec{
				Labels: []apiv1.ContainerLabel{
					{
						Key:   controllers.PersistentLabel,
						Value: "false",
					},
					{
						Key:   controllers.CreatorProcessIdLabel,
						Value: fmt.Sprintf("%d", procNonExistent.Pid),
					},
					{
						Key:   controllers.CreatorProcessStartTimeLabel,
						Value: procNonExistent.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
					},
				},
			},
		},
	})
	require.NoError(t, containerCreateErr)

	// Network that has no containers attached to it (should be removed)
	const abandonedNetworkNoContainers = prefix + "abandoned-no-containers"
	_, netCreateErr = co.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: abandonedNetworkNoContainers,
		Labels: map[string]string{
			controllers.PersistentLabel:              "false",
			controllers.CreatorProcessIdLabel:        fmt.Sprintf("%d", procNonExistent.Pid),
			controllers.CreatorProcessStartTimeLabel: procNonExistent.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
		},
	})
	require.NoError(t, netCreateErr)

	// Network that has only DCP abandoned, non-persistent containers attached to it (should be removed)
	const protectedPersistentNetwork = prefix + "persistent-protected"
	_, netCreateErr = co.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: protectedPersistentNetwork,
		Labels: map[string]string{
			controllers.PersistentLabel:              "false",
			controllers.CreatorProcessIdLabel:        fmt.Sprintf("%d", procNonExistent.Pid),
			controllers.CreatorProcessStartTimeLabel: procNonExistent.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
		},
	})
	require.NoError(t, netCreateErr)

	harvester := controllers.NewResourceHarvester()
	require.True(t, harvester.TryProtectNetwork(ctx, protectedPersistentNetwork), "could not protect network")
	harvester.Harvest(ctx, co, log)

	remaining, listNetworksErr := co.ListNetworks(ctx, containers.ListNetworksOptions{})
	require.NoError(t, listNetworksErr, "could not list networks")
	remainingNames := slices.Map[containers.ListedNetwork, string](remaining, func(n containers.ListedNetwork) string {
		return n.Name
	})
	require.ElementsMatch(t, []string{
		netNoLabels,
		netPersistentWithContainer,
		netUsedByExistingProcess,
		netWithNonDcpContainers,
		netWithMixedContainers,
		netWithAbandonedPersistentContainer,
		protectedPersistentNetwork,
		co.DefaultNetworkName(),
		"host",
		"none",
	}, remainingNames, "unexpected networks remaining")
}
