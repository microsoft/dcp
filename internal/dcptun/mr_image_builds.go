/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcptun

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/lockfile"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/slices"
)

const (
	imageBuildsRecordPattern = "%s %s %s %s" // imageName baseImageDigest programInstanceID timestamp
)

var (
	getDefaultImageBuildsFile = sync.OnceValues(createPackageImageBuildsFile)
)

// A record representing one of the most recent client proxy image builds.
type imageBuildRecord struct {
	ImageName       string      // The name and tag of the image
	BaseImageDigest imageDigest // The digest of the base image used for the build
	Instance        string      // The DCP instance ID that is building (or should have built) the image
	Timestamp       time.Time
}

type imageBuildRecordMarshaller struct{}

func (_ imageBuildRecordMarshaller) Unmarshal(line []byte) (imageBuildRecord, error) {
	var imageName, instance, timestampStr string
	var baseImageDigest imageDigest
	n, scanErr := fmt.Fscanf(bytes.NewReader(line), imageBuildsRecordPattern, &imageName, &baseImageDigest, &instance, &timestampStr)
	if scanErr != nil {
		return imageBuildRecord{}, fmt.Errorf("the most recent image builds file is corrupted (invalid record, the line read was '%s'): %w", line, scanErr)
	}
	if n != 4 {
		return imageBuildRecord{}, fmt.Errorf("the most recent image builds file is corrupted (invalid record, expected 4 fields but got %d, the line was '%s')", n, line)
	}

	timestamp, timestampParseErr := time.Parse(time.RFC3339Nano, timestampStr)
	if timestampParseErr != nil {
		return imageBuildRecord{}, fmt.Errorf("the most recent image builds file is corrupted (invalid timestamp found, the timestamp string was '%s')", timestampStr)
	}

	imageName = strings.TrimSpace(imageName)
	baseImageDigest = imageDigest(strings.TrimSpace(string(baseImageDigest)))
	instance = strings.TrimSpace(instance)

	if imageName == "" {
		return imageBuildRecord{}, fmt.Errorf("the most recent image builds file is corrupted (image name cannot be empty, the line was '%s')", line)
	}
	if baseImageDigest == "" {
		return imageBuildRecord{}, fmt.Errorf("the most recent image builds file is corrupted (base image digest cannot be empty, the line was '%s')", line)
	}
	if instance == "" {
		return imageBuildRecord{}, fmt.Errorf("the most recent image builds file is corrupted (instance ID cannot be empty, the line was '%s')", line)
	}

	return imageBuildRecord{
		ImageName:       imageName,
		BaseImageDigest: baseImageDigest,
		Instance:        instance,
		Timestamp:       timestamp,
	}, nil
}

func (_ imageBuildRecordMarshaller) Marshal(record imageBuildRecord) []byte {
	return fmt.Appendf(nil, imageBuildsRecordPattern,
		strings.TrimSpace(record.ImageName),
		strings.TrimSpace(string(record.BaseImageDigest)),
		strings.TrimSpace(record.Instance),
		record.Timestamp.Format(time.RFC3339Nano),
	)
}

type imageBuildsFile struct {
	lockfile.RecordFile[imageBuildRecord]
}

func newImageBuildsFile(path string) (*imageBuildsFile, error) {
	recordFile, err := lockfile.NewRecordFile(path, imageBuildRecordMarshaller{})
	if err != nil {
		return nil, err
	}
	return &imageBuildsFile{
		RecordFile: *recordFile,
	}, nil
}

// Returns the content of the most recent image builds file, with expired records removed.
// The file is left locked if the operation is successful.
// If an error occurs, the file is truncated and unlocked.
func (imf *imageBuildsFile) tryLockAndRead(ctx context.Context, ttl time.Duration) ([]imageBuildRecord, error) {
	records, readErr := imf.TryLockAndRead(ctx)
	if readErr != nil {
		return nil, readErr
	}

	records = slices.Select(records, func(r imageBuildRecord) bool {
		return time.Since(r.Timestamp) < ttl
	})

	return records, nil
}

func createPackageImageBuildsFile() (*imageBuildsFile, error) {
	dcpFolder, dcpFolderErr := dcppaths.EnsureUserDcpDir()
	if dcpFolderErr != nil {
		return nil, dcpFolderErr
	}

	isAdmin, isAdminErr := osutil.IsAdmin()
	if isAdminErr != nil {
		return nil, isAdminErr
	}

	var filePath string
	if isAdmin {
		filePath = filepath.Join(dcpFolder, "mrImageBuilds.elevated.list")
	} else {
		filePath = filepath.Join(dcpFolder, "mrImageBuilds.list")
	}

	packageImageBuildsFile, creationErr := newImageBuildsFile(filePath)
	return packageImageBuildsFile, creationErr
}
