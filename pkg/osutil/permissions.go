package osutil

import "os"

const (
	PermissionOwnerReadWriteOthersRead     os.FileMode = 0644
	PermissionOnlyOwnerReadWrite           os.FileMode = 0600
	PermissionOnlyOwnerReadWriteSetCurrent os.FileMode = 0700 // For directories
	PermissionOnlyOwnerReadWriteExecute    os.FileMode = 0700 // For files
)
