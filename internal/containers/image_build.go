/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

const defaultBuildImageTimeout = 10 * time.Minute

// BuildImageImpl builds a container image using the container runtime CLI.
// Additional arguments are appended immediately before the build context argument.
func BuildImageImpl(
	ctx context.Context,
	options BuildImageOptions,
	runner CLICommandRunner,
	additionalArgs ...string,
) (*bytes.Buffer, error) {
	if options.ContainerBuildContext == nil {
		return nil, fmt.Errorf("container build context is required")
	}
	if options.Context == "" && options.ContextArchive == nil {
		return nil, fmt.Errorf("build context path or build context archive is required")
	}
	if options.Context != "" && options.ContextArchive != nil {
		return nil, fmt.Errorf("build context path and build context archive are mutually exclusive")
	}

	args := []string{"build"}

	if options.Dockerfile != "" {
		args = append(args, "-f", options.Dockerfile)
	}

	if options.Pull {
		args = append(args, "--pull")
	}

	if options.IidFile != "" {
		args = append(args, "--iidfile", options.IidFile)
	}

	for _, tag := range options.Tags {
		args = append(args, "-t", tag)
	}

	for _, buildArg := range options.Args {
		if buildArg.Value != "" {
			args = append(args, "--build-arg", fmt.Sprintf("%s=%s", buildArg.Name, buildArg.Value))
		} else {
			args = append(args, "--build-arg", buildArg.Name)
		}
	}

	secretEnvironment := map[string]string{}
	for _, secret := range options.Secrets {
		switch secret.Type {
		case FileSecret, "":
			args = append(args, "--secret", fmt.Sprintf("id=%s,src=%s", secret.ID, secret.Source))
		case EnvSecret:
			secretSource := secret.Source
			if secretSource == "" {
				secretSource = secret.ID
			}
			args = append(args, "--secret", fmt.Sprintf("id=%s,env=%s", secret.ID, secretSource))
			if secret.Value != "" {
				secretEnvironment[secretSource] = secret.Value
			}
		}
	}

	if options.Stage != "" {
		args = append(args, "--target", options.Stage)
	}

	for _, label := range options.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", label.Key, label.Value))
	}

	if options.Platform != "" {
		args = append(args, "--platform", options.Platform)
	}

	args = append(args, additionalArgs...)

	buildContextArgument := options.Context
	var buildContextArchive io.ReadCloser
	if options.ContextArchive != nil {
		var archiveErr error
		buildContextArchive, archiveErr = OpenBuildContextArchive(options.ContextArchive)
		if archiveErr != nil {
			return nil, archiveErr
		}
		defer buildContextArchive.Close()
		buildContextArgument = "-"
	}
	args = append(args, buildContextArgument)

	cmd := runner.MakeCommand(args...)
	if buildContextArchive != nil {
		cmd.Stdin = buildContextArchive
	}

	cmd.Env = os.Environ()
	for secretName, secretValue := range secretEnvironment {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", secretName, secretValue))
	}

	timeout := options.Timeout
	if timeout == 0 {
		timeout = defaultBuildImageTimeout
	}

	_, errBuf, buildErr := runner.RunBufferedCommand(ctx, "BuildImage", cmd, options.StdOutStream, options.StdErrStream, timeout)
	return errBuf, buildErr
}
