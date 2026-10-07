//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"fmt"
	"math"
	"syscall"

	"golang.org/x/sys/unix"
)

// FindProcessGroup captures the group led by handle after verifying its process identity.
// It returns nil if the process is not a group leader. The caller's own group is never a valid target.
func FindProcessGroup(handle ProcessHandle) (*ProcessGroup, error) {
	if handle.Pid <= 1 || handle.Pid > math.MaxInt32 {
		return nil, fmt.Errorf("invalid process group leader PID %d", handle.Pid)
	}
	_, processErr := findProcessInfo(handle)
	if processErr != nil {
		return nil, processErr
	}
	groupID, groupErr := unix.Getpgid(int(handle.Pid))
	if groupErr != nil {
		if errors.Is(groupErr, syscall.ESRCH) {
			return nil, fmt.Errorf("process %d exited: %w", handle.Pid, ErrorProcessNotFound)
		}
		return nil, fmt.Errorf("could not get process group for process %d: %w", handle.Pid, groupErr)
	}
	if groupID != int(handle.Pid) {
		return nil, nil
	}
	if groupID == unix.Getpgrp() {
		return nil, fmt.Errorf("cannot target the current process group %d", groupID)
	}
	if _, identityErr := findProcessInfo(handle); identityErr != nil {
		return nil, identityErr
	}
	return &ProcessGroup{leader: handle, WaitPollInterval: defaultWaitPollInterval}, nil
}

func (g *ProcessGroup) validateLeader() error {
	if g.leader.Pid <= 1 || g.leader.Pid > math.MaxInt32 || g.leader.IdentityTime.IsZero() {
		return fmt.Errorf("invalid process group leader identity for PID %d", g.leader.Pid)
	}
	if int(g.leader.Pid) == unix.Getpgrp() {
		return fmt.Errorf("cannot target the current process group %d", g.leader.Pid)
	}
	leader, leaderErr := readProcessInfo(g.leader.Pid)
	if leaderErr != nil {
		if errors.Is(leaderErr, ErrorProcessNotFound) {
			// A group can survive its original leader, but a reused leader PID must not be targeted.
			return nil
		}
		return leaderErr
	}
	if identityErr := validateIdentity(g.leader, leader.handle); identityErr != nil {
		return identityErr
	}
	groupID, groupErr := unix.Getpgid(int(g.leader.Pid))
	if errors.Is(groupErr, syscall.ESRCH) {
		return nil
	}
	if groupErr != nil {
		return fmt.Errorf("could not verify process group %d: %w", g.leader.Pid, groupErr)
	}
	if groupID != int(g.leader.Pid) {
		return fmt.Errorf("process %d no longer identifies an isolated target group", g.leader.Pid)
	}
	return nil
}

func (g *ProcessGroup) signal(ctx context.Context, signal syscall.Signal) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if identityErr := g.validateLeader(); identityErr != nil {
		return identityErr
	}
	signalErr := unix.Kill(-int(g.leader.Pid), signal)
	if errors.Is(signalErr, syscall.ESRCH) {
		return nil
	}
	if errors.Is(signalErr, syscall.EPERM) {
		// Darwin can return EPERM when a group has no signalable members.
		running, runningErr := g.isRunning(ctx)
		if runningErr != nil {
			return errors.Join(signalErr, runningErr)
		}
		if !running {
			return nil
		}
	}
	if signalErr != nil {
		return fmt.Errorf("could not send signal %s to process group %d: %w", signal.String(), g.leader.Pid, signalErr)
	}
	return nil
}

func (g *ProcessGroup) isRunning(ctx context.Context) (bool, error) {
	if identityErr := g.validateLeader(); identityErr != nil {
		return false, identityErr
	}
	members, membersErr := g.members(ctx)
	if membersErr != nil {
		return false, membersErr
	}
	zombies := make(map[ProcessHandle]struct{}, len(members))
	for _, member := range members {
		if !member.exited {
			return true, nil
		}
		zombies[member.handle] = struct{}{}
	}

	// A member can fork and exit during enumeration. Only known zombies may remain in the final census.
	remainingMembers, remainingErr := g.members(ctx)
	if remainingErr != nil {
		return false, remainingErr
	}
	for _, remaining := range remainingMembers {
		if _, zombie := zombies[remaining.handle]; !zombie || !remaining.exited {
			return true, nil
		}
	}
	return false, nil
}

func (g *ProcessGroup) members(ctx context.Context) ([]processInfo, error) {
	processes, snapshotErr := snapshotProcesses(ctx)
	if snapshotErr != nil {
		return nil, fmt.Errorf("could not enumerate process group %d: %w", g.leader.Pid, snapshotErr)
	}
	members := make([]processInfo, 0)
	for _, info := range processes {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		groupID, groupErr := unix.Getpgid(int(info.handle.Pid))
		if errors.Is(groupErr, syscall.ESRCH) {
			continue
		}
		if groupErr != nil {
			return nil, fmt.Errorf("could not get process group for process %d: %w", info.handle.Pid, groupErr)
		}
		if groupID == int(g.leader.Pid) {
			members = append(members, info)
		}
	}
	return members, nil
}
