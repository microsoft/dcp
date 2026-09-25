/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"fmt"
)

// GetProcessTree returns the root and verified descendants in breadth-first order.
// ErrIncompleteProcessTree accompanies a usable partial result; other errors invalidate the result.
// The snapshot does not include subsequently created children, and every action still requires identity validation.
func GetProcessTree(ctx context.Context, root ProcessHandle) ([]ProcessHandle, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	before, beforeErr := findProcessInfo(root)
	if beforeErr != nil {
		return nil, beforeErr
	}
	snapshot, snapshotErr := snapshotProcesses(ctx)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	after, afterErr := findProcessInfo(before.handle)
	if afterErr != nil {
		return nil, afterErr
	}
	if before.birth != after.birth {
		return nil, fmt.Errorf("%w during enumeration for pid %d", ErrProcessIdentityMismatch, root.Pid)
	}
	tree, treeErr := buildProcessTree(ctx, before, snapshot)
	if treeErr != nil && !errors.Is(treeErr, ErrIncompleteProcessTree) {
		return nil, treeErr
	}
	if snapshotErr != nil {
		return tree, errors.Join(treeErr, fmt.Errorf("%w: %w", ErrIncompleteProcessTree, snapshotErr))
	}
	return tree, treeErr
}

func buildProcessTree(ctx context.Context, root processInfo, snapshot []processInfo) ([]ProcessHandle, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if handleErr := root.handle.Validate(); handleErr != nil {
		return nil, handleErr
	}
	byPID := make(map[Pid_t]processInfo, len(snapshot))
	ambiguous := make(map[Pid_t]bool)
	for _, info := range snapshot {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		if previous, exists := byPID[info.handle.Pid]; exists && previous != info {
			ambiguous[info.handle.Pid] = true
		}
		byPID[info.handle.Pid] = info
	}
	snapshotRoot, rootFound := byPID[root.handle.Pid]
	if !rootFound || ambiguous[root.handle.Pid] {
		return []ProcessHandle{root.handle}, fmt.Errorf("%w: missing or ambiguous root record", ErrIncompleteProcessTree)
	}
	if identityErr := validateIdentity(root.handle, snapshotRoot.handle); identityErr != nil {
		return nil, identityErr
	}
	if root.birth != snapshotRoot.birth {
		return nil, fmt.Errorf("%w in snapshot for pid %d", ErrProcessIdentityMismatch, root.handle.Pid)
	}

	parents := make(map[ProcessHandle]ProcessHandle, len(snapshot))
	children := make(map[ProcessHandle][]ProcessHandle)
	uncertain := make(map[ProcessHandle][]error)
	for _, child := range snapshot {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		parent, parentFound := byPID[child.parentPID]
		if !parentFound || ambiguous[parent.handle.Pid] || parent.handle.Validate() != nil {
			continue
		}
		if ambiguous[child.handle.Pid] || child.handle.Validate() != nil {
			uncertain[parent.handle] = append(uncertain[parent.handle],
				fmt.Errorf("unverifiable descendant pid %d", child.handle.Pid))
			continue
		}
		if child.birth < parent.birth {
			continue
		}
		if _, alreadyLinked := parents[child.handle]; alreadyLinked {
			continue
		}
		parents[child.handle] = parent.handle
		children[parent.handle] = append(children[parent.handle], child.handle)
	}

	cyclic, cycleErr := cyclicProcessIdentities(ctx, parents)
	if cycleErr != nil {
		return nil, cycleErr
	}
	tree := []ProcessHandle{snapshotRoot.handle}
	visited := map[ProcessHandle]bool{snapshotRoot.handle: true}
	var issues []error
	for head := 0; head < len(tree); head++ {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		parent := tree[head]
		issues = append(issues, uncertain[parent]...)
		for _, child := range children[parent] {
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			if cyclic[child] {
				issues = append(issues, fmt.Errorf("cyclic ancestry involving pid %d", child.Pid))
				continue
			}
			if visited[child] {
				continue
			}
			visited[child] = true
			tree = append(tree, child)
		}
	}
	if len(issues) != 0 {
		return tree, fmt.Errorf("%w: %w", ErrIncompleteProcessTree, errors.Join(issues...))
	}
	return tree, nil
}

func cyclicProcessIdentities(ctx context.Context, parents map[ProcessHandle]ProcessHandle) (map[ProcessHandle]bool, error) {
	finished := make(map[ProcessHandle]bool, len(parents))
	cyclic := make(map[ProcessHandle]bool)
	for initial := range parents {
		if finished[initial] {
			continue
		}
		path := []ProcessHandle{}
		position := make(map[ProcessHandle]int)
		current := initial
		for !finished[current] {
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			if cycleStart, onPath := position[current]; onPath {
				for _, member := range path[cycleStart:] {
					cyclic[member] = true
				}
				break
			}
			position[current] = len(path)
			path = append(path, current)
			parent, hasParent := parents[current]
			if !hasParent {
				break
			}
			current = parent
		}
		for _, member := range path {
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			finished[member] = true
		}
	}
	return cyclic, nil
}
