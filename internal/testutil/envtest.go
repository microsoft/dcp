package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

const ()

func GetKubeAssetsDir() (string, error) {
	// KUBEBUILDER_ASSETS is a well-known environment variable used to configure envtest
	assetsDirectory := os.Getenv("KUBEBUILDER_ASSETS")
	if assetsDirectory != "" {
		return assetsDirectory, nil
	}

	// If KUBEBUILDER_ASSETS is not set, we'll try to find the assets directory using the setup-envtest tool
	// First read the Kubernetes version to use from Makefile
	cmd := exec.Command(
		"awk",
		`/ENVTEST_K8S_VERSION\s*=/ { print $3; exit 0 }`,
		"./Makefile",
	)
	dir, err := testutil.FindRootFor(testutil.FileTarget, "Makefile")
	if err != nil {
		return "", fmt.Errorf("could not find the path to Makefile: %w", err)
	}
	cmd.Dir = dir

	targetKubeVersion, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to read Kubernetes version (ENVTEST_K8S_VERSION) from Makefile: %w", err)
	}

	const toolBinDir = ".toolbin"
	var setupEnvtestExe = "setup-envtest"
	if runtime.GOOS == "windows" {
		setupEnvtestExe += ".exe"
	}
	dir, err = testutil.FindRootFor(testutil.FileTarget, toolBinDir, setupEnvtestExe)
	if err != nil {
		return "", fmt.Errorf("could not find '%s' executable", setupEnvtestExe)
	}

	cmd = exec.Command(
		filepath.Join(dir, toolBinDir, setupEnvtestExe),
		"use",
		"-p", "path",
		strings.TrimSpace(string(targetKubeVersion)),
	)
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("the call to '%s' to find path Kubernetes assets path failed: %w", setupEnvtestExe, err)
	} else {
		return string(out), err
	}
}
