/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package osutil

import "os"

const (
	PermissionOwnerReadWriteOthersRead   os.FileMode = 0644
	PermissionOnlyOwnerReadWrite         os.FileMode = 0600
	PermissionOnlyOwnerReadWriteTraverse os.FileMode = 0700 // For directories
	PermissionDirectoryOthersRead        os.FileMode = 0755 // For shared directories
	PermissionOnlyOwnerReadWriteExecute  os.FileMode = 0700 // For files
	PermissionCheckEveryoneRead          os.FileMode = 0004 // Everyone can read, used for checking permissions
	PermissionCheckEveryoneWrite         os.FileMode = 0002 // Everyone can write, used for checking permissions
	PermissionCheckEveryoneExecute       os.FileMode = 0001 // Everyone can execute, used for checking permissions

	// Bitmask combined with umask for creating directories. This represents the maximum possible default permissions
	// for a folder; the final default permissions are determined by masking (subtracting) the umask bits from these
	// bits.
	DefaultFolderBitmask os.FileMode = 0777

	// Bitmask combined with umask for creating files. This represents the maximum possible default permissions for a
	// file; the final default permissions are determined by masking (subtracting) the umask bits from these bits.
	DefaultFileBitmask os.FileMode = 0666
	// Default umask for creating files and directories. The umask represents permissions that should be denied
	// when using the default file and directory permissions. The umask is subtracted from the maximum possible
	// permissions, so a umask of 022 would result in default folder permissions of 0755 (0777 - 022) and default file
	// permissions of 0644 (0666 - 022).
	DefaultUmaskBitmask os.FileMode = 022
)
