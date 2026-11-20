/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package concurrency

func InspectLeaseMapEntryCountForTest[Key comparable](lm *LeaseMap[Key]) int {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	return len(lm.entries)
}
