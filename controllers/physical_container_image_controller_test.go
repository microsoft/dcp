/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stretchr/testify/require"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

func TestPhysicalContainerImageIDFileNameUsesUID(t *testing.T) {
	t.Parallel()

	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.Repeat("a", 253),
			Namespace: "test",
			UID:       types.UID("image-uid"),
		},
	}

	require.Equal(t, "pci_iid_image-uid", physicalContainerImageIDFileName(image))
}

func TestPhysicalContainerImageIDFileNameBoundsMissingUIDFallback(t *testing.T) {
	t.Parallel()

	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.Repeat("a", 253),
			Namespace: "test",
		},
	}

	require.LessOrEqual(t, len(physicalContainerImageIDFileName(image)), 255)
}
