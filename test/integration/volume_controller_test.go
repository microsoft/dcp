package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

func TestVolumeCreation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "volume-creation"
	volumeID := testName + "-" + testutil.GetRandLetters(t, 6)

	vol := apiv1.ContainerVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerVolumeSpec{
			Name: volumeID,
		},
	}

	t.Logf("Creating ContainerVolume object '%s'...", vol.ObjectMeta.Name)
	err := client.Create(ctx, &vol)
	require.NoError(t, err, "Could not create a ContainerVolume object")

	t.Log("Ensure that a corresponding Docker volume was created...")
	err = ensureVolumeCreated(t, ctx, volumeID)
	require.NoError(t, err)
}

func TestVolumeDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "volume-deletion"
	volumeID := testName + "-" + testutil.GetRandLetters(t, 6)

	vol := apiv1.ContainerVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerVolumeSpec{
			Name: volumeID,
		},
	}

	t.Logf("Creating ContainerVolume object '%s'...", vol.ObjectMeta.Name)
	err := client.Create(ctx, &vol)
	require.NoError(t, err, "Could not create a ContainerVolume object")

	t.Log("Ensure that a corresponding Docker volume was created...")
	err = ensureVolumeCreated(t, ctx, volumeID)
	require.NoError(t, err)

	t.Log("Deleting ContainerVolume object...")
	err = client.Delete(ctx, &vol)
	require.NoError(t, err, "ContainerVolume object could not be deleted")

	t.Log("Check if corresponding volume was deleted...")
	err = ensureVolumeDeleted(t, ctx, volumeID)
	require.NoError(t, err, "Volume was not deleted as expected")

	t.Log("Ensure that ContainerVolume object really disappeared from the API server...")
	waitObjectDeleted[apiv1.ContainerVolume](t, ctx, ctrl_client.ObjectKeyFromObject(&vol))
}

func ensureVolumeCreated(t *testing.T, ctx context.Context, volumeID string) error {
	// The controller will try to determine if the volume exists, and if not, create the volume
	volInspectCmd, err := waitForDockerCommand(t, ctx, []string{"volume", "inspect"}, volumeID)
	if err != nil {
		return err
	}

	// Indicate that the volume does not exist
	// Note that the error message goes to stderr, not stdout (consistent with how Docker CLI does it)
	_, err = volInspectCmd.Cmd.Stderr.Write([]byte(fmt.Sprintf("Error: No such volume: %s\n", volumeID)))
	if err != nil {
		t.Errorf("Could not report missing volume: %v", err)
		return err
	}
	processExecutor.SimulateProcessExit(t, volInspectCmd.PID, 1)

	// Expect volume creation command
	volCreateCmd, err := waitForDockerCommand(t, ctx, []string{"volume", "create"}, volumeID)
	if err != nil {
		return err
	}

	// Confirm volume creation
	_, err = volCreateCmd.Cmd.Stdout.Write([]byte(volumeID + "\n"))
	if err != nil {
		t.Errorf("Could not confirm volume creation: %v", err)
		return err
	}
	processExecutor.SimulateProcessExit(t, volCreateCmd.PID, 0)
	created := time.Now().UTC()

	// The controller will want to inspect the created volume
	volInspectCmd, err = waitForDockerCommand(t, ctx, []string{"volume", "inspect"}, volumeID)
	if err != nil {
		return err
	}

	// Indicate that the volume does exist now
	inspectRes := getInspectedVolumeJson(volumeID, created) + "\n"
	_, err = volInspectCmd.Cmd.Stdout.Write([]byte(inspectRes))
	if err != nil {
		t.Errorf("Could not provide volume inspection result: %v", err)
		return err
	}
	processExecutor.SimulateProcessExit(t, volInspectCmd.PID, 0)

	return nil
}

func ensureVolumeDeleted(t *testing.T, ctx context.Context, volumeID string) error {
	removeCmd, err := waitForDockerCommand(t, ctx, []string{"volume", "rm"}, volumeID)
	if err != nil {
		return err
	}

	// Reply with volume ID to confirm volume removal
	_, err = removeCmd.Cmd.Stdout.Write([]byte(volumeID + "\n"))
	if err != nil {
		t.Errorf("Could not confirm volume removal: %v", err)
		return err
	}
	processExecutor.SimulateProcessExit(t, removeCmd.PID, 0)

	return nil
}

func getInspectedVolumeJson(volumeID string, created time.Time) string {
	retval := fmt.Sprintf(`{
		"Name": "%s",
		"CreatedAt": "%s",
		"Driver": "local",
		"Labels": {},
		"Mountpoint": "/var/lib/docker/volumes/%s/_data",
		"Options": {},
		"Scope": "local"
	}`, volumeID, created.Format(time.RFC3339), volumeID)

	return strings.ReplaceAll(retval, "\n", " ") // Make it a single-line (JSON lines) record
}
