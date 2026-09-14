/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/microsoft/dcp/internal/containers"
)

func (wco *WslcCliOrchestrator) CreateVolume(ctx context.Context, options containers.CreateVolumeOptions) error {
	if options.Name == "" {
		return fmt.Errorf("must specify a volume name")
	}

	args := []string{"volume", "create"}
	labelKeys := make([]string, 0, len(options.Labels))
	for key := range options.Labels {
		labelKeys = append(labelKeys, key)
	}
	sort.Strings(labelKeys)
	for _, key := range labelKeys {
		if key == "" {
			return fmt.Errorf("volume label key cannot be empty")
		}
		args = append(args, "--label", key+"="+options.Labels[key])
	}
	args = append(args, options.Name)

	cmd := makeWslcCommand(args...)
	outBuf, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"CreateVolume",
		cmd,
		nil,
		nil,
		ordinaryCommandTimeout,
	)
	if runErr != nil {
		return errors.Join(runErr, normalizeCliErrors(errBuf, alreadyExistsMatch))
	}

	outputName, outputErr := parseSingleIdentifier(outBuf)
	if outputErr != nil {
		return outputErr
	}
	if outputName != options.Name {
		return fmt.Errorf("wslc volume create returned name %q instead of %q", outputName, options.Name)
	}
	return nil
}

func (wco *WslcCliOrchestrator) InspectVolumes(ctx context.Context, options containers.InspectVolumesOptions) ([]containers.InspectedVolume, error) {
	if len(options.Volumes) == 0 {
		return nil, fmt.Errorf("must specify at least one volume")
	}

	args := append([]string{"volume", "inspect", "--format", "json"}, options.Volumes...)
	cmd := makeWslcCommand(args...)
	outBuf, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"InspectVolumes",
		cmd,
		nil,
		nil,
		ordinaryCommandTimeout,
	)

	rawVolumes, decodeErr := decodeJSONArray[wslcInspectedVolume](outBuf)
	if runErr != nil {
		runErr = errors.Join(runErr, normalizeCliErrors(errBuf, volumeNotFoundMatch.MaxObjects(len(options.Volumes))))
	}

	inspectedVolumes := make([]containers.InspectedVolume, 0, len(rawVolumes))
	var conversionErr error
	for _, rawVolume := range rawVolumes {
		volumeName := strings.TrimSpace(rawVolume.Name)
		if volumeName == "" {
			conversionErr = errors.Join(
				conversionErr,
				containers.ErrUnmarshalling,
				fmt.Errorf("inspected WSLC volume did not contain a name"),
			)
			continue
		}
		inspectedVolumes = append(inspectedVolumes, containers.InspectedVolume{
			Name:       volumeName,
			Driver:     rawVolume.Driver,
			MountPoint: rawVolume.Mountpoint,
			Scope:      rawVolume.Scope,
			Labels:     rawVolume.Labels,
			CreatedAt:  rawVolume.CreatedAt.Time,
		})
	}

	return inspectedVolumes, errors.Join(
		runErr,
		decodeErr,
		conversionErr,
		incompleteError("volumes", len(inspectedVolumes), len(options.Volumes)),
	)
}

func (wco *WslcCliOrchestrator) ListVolumes(ctx context.Context, options containers.ListVolumesOptions) ([]containers.ListedVolume, error) {
	args := []string{"volume", "list"}
	for _, label := range options.Filters.LabelFilters {
		filter := "label=" + label.Key
		if label.Value != "" {
			filter += "=" + label.Value
		}
		args = append(args, "--filter", filter)
	}
	args = append(args, "--format", "json")

	cmd := makeWslcCommand(args...)
	outBuf, errBuf, runErr := wco.runBufferedWslcCommand(
		ctx,
		"ListVolumes",
		cmd,
		nil,
		nil,
		ordinaryCommandTimeout,
	)
	if runErr != nil {
		return nil, errors.Join(runErr, normalizeCliErrors(errBuf))
	}

	rawVolumes, decodeErr := decodeJSONLines[wslcListedVolume](outBuf)
	listedVolumes := make([]containers.ListedVolume, 0, len(rawVolumes))
	for _, rawVolume := range rawVolumes {
		volumeName := strings.TrimSpace(rawVolume.Name)
		if volumeName == "" {
			decodeErr = errors.Join(
				decodeErr,
				containers.ErrUnmarshalling,
				fmt.Errorf("listed WSLC volume did not contain a name"),
			)
			continue
		}
		listedVolumes = append(listedVolumes, containers.ListedVolume{Name: volumeName})
	}
	return listedVolumes, decodeErr
}

func (wco *WslcCliOrchestrator) RemoveVolumes(ctx context.Context, options containers.RemoveVolumesOptions) ([]string, error) {
	if len(options.Volumes) == 0 {
		return nil, fmt.Errorf("must specify at least one volume")
	}

	return runSequentially(ctx, "volumes", options.Volumes, func(volumeReference string) error {
		args := []string{"volume", "remove"}
		if options.Force {
			args = append(args, "--force")
		}
		args = append(args, volumeReference)

		cmd := makeWslcCommand(args...)
		_, errBuf, runErr := wco.runBufferedWslcCommand(
			ctx,
			"RemoveVolume",
			cmd,
			nil,
			nil,
			ordinaryCommandTimeout,
		)
		if runErr != nil {
			return errors.Join(runErr, normalizeCliErrors(errBuf, volumeNotFoundMatch, objectInUseMatch))
		}
		return nil
	})
}
