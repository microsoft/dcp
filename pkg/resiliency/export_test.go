/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package resiliency

func InspectPartitionedQueueStateForTest[P comparable](pwq *PartitionedWorkQueue[P]) (int, uint, uint) {
	pwq.lock.Lock()
	defer pwq.lock.Unlock()
	return len(pwq.partitions), pwq.totalRunning, pwq.pendingCount
}
