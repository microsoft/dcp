package osutil

import "os"

const (
	PermissionOwnerReadWriteOthersRead     os.FileMode = 0644
	PermissionOnlyOwnerReadWrite           os.FileMode = 0600
	PermissionOnlyOwnerReadWriteSetCurrent os.FileMode = 0700 // For directories
	PermissionDirectoryOthersRead          os.FileMode = 0755 // For shared directories
	PermissionOnlyOwnerReadWriteExecute    os.FileMode = 0700 // For files
	PermissionCheckEveryoneRead            os.FileMode = 0004 // Everyone can read, used for checking permissions
	PermissionCheckEveryoneWrite           os.FileMode = 0002 // Everyone can write, used for checking permissions
	PermissionCheckEveryoneExecute         os.FileMode = 0001 // Everyone can execute, used for checking permissions
)
