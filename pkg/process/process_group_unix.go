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

// FindProcessGroup identifies the group led by handle.
// It returns nil if the process is not a group leader. The caller's own group is never a valid target.
// Set knownLeader only for a process known to have been launched as an isolated group leader;
// its handle then identifies the group even after the leader exits, with identity checked by group operations.
func FindProcessGroup(handle ProcessHandle, knownLeader bool) (*ProcessGroup, error) {
	if handleErr := handle.Validate(); handleErr != nil {
		return nil, handleErr
	}
	pid, pidErr := PidT_ToInt(handle.Pid)
	if pidErr != nil {
		return nil, pidErr
	}
	if handle.Pid <= 1 || handle.Pid > math.MaxInt32 {
		return nil, fmt.Errorf("invalid process group leader PID %d", handle.Pid)
	}
	if pid == unix.Getpgrp() {
		return nil, fmt.Errorf("cannot target the current process group %d", pid)
	}
	if knownLeader {
		return &ProcessGroup{leader: handle, WaitPollInterval: defaultWaitPollInterval}, nil
	}
	info, processErr := handle.findProcessInfo()
	if processErr != nil {
		return nil, processErr
	}
	if info.groupID != handle.Pid {
		return nil, nil
	}
	if _, identityErr := handle.findProcessInfo(); identityErr != nil {
		return nil, identityErr
	}
	return &ProcessGroup{leader: handle, WaitPollInterval: defaultWaitPollInterval}, nil
}

func (g *ProcessGroup) hasLiveLeader() (bool, error) {
	leaderPID, leaderPIDErr := PidT_ToInt(g.leader.Pid)
	if leaderPIDErr != nil {
		return false, leaderPIDErr
	}
	if g.leader.Pid <= 1 || g.leader.Pid > math.MaxInt32 || g.leader.IdentityTime.IsZero() {
		return false, fmt.Errorf("invalid process group leader identity for PID %d", g.leader.Pid)
	}
	if leaderPID == unix.Getpgrp() {
		return false, fmt.Errorf("cannot target the current process group %d", g.leader.Pid)
	}
	leader, leaderErr := readProcessInfo(g.leader.Pid)
	if leaderErr != nil {
		if IsProcessGoneErr(leaderErr) {
			// A group can survive its original leader, but a reused leader PID must not be targeted.
			return false, nil
		}
		return false, leaderErr
	}
	if identityErr := validateIdentity(g.leader, leader.handle); identityErr != nil {
		return false, identityErr
	}
	if leader.groupID != g.leader.Pid {
		return false, fmt.Errorf("process %d no longer identifies an isolated target group", g.leader.Pid)
	}
	return !leader.exited, nil
}

func (g *ProcessGroup) signal(ctx context.Context, signal syscall.Signal) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if _, identityErr := g.hasLiveLeader(); identityErr != nil {
		if errors.Is(identityErr, ErrProcessIdentityMismatch) {
			// A leader PID cannot be reused while the original group exists. Never signal its replacement.
			return nil
		}
		return identityErr
	}
	groupID, groupIDErr := PidT_ToInt(g.leader.Pid)
	if groupIDErr != nil {
		return groupIDErr
	}
	signalErr := unix.Kill(-groupID, signal)
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
	if contextErr := ctx.Err(); contextErr != nil {
		return false, contextErr
	}
	leaderRunning, identityErr := g.hasLiveLeader()
	if identityErr != nil {
		if errors.Is(identityErr, ErrProcessIdentityMismatch) {
			return false, nil
		}
		return false, identityErr
	}
	if leaderRunning {
		return true, nil
	}
	return isProcessGroupRunning(ctx, g.leader.Pid, snapshotProcesses)
}

func isProcessGroupRunning(
	ctx context.Context,
	groupID Pid_t,
	snapshotAll func(context.Context) ([]processInfo, error),
) (bool, error) {
	zombies := make(map[ProcessHandle]struct{})
	// A member can fork and exit during enumeration. Only known zombies may remain in the final census.
	for snapshotIndex := range 2 {
		if contextErr := ctx.Err(); contextErr != nil {
			return false, contextErr
		}
		processes, snapshotErr := snapshotAll(ctx)
		if contextErr := ctx.Err(); contextErr != nil {
			return false, contextErr
		}
		newZombie := false
		for _, info := range processes {
			if contextErr := ctx.Err(); contextErr != nil {
				return false, contextErr
			}
			if info.groupID != groupID {
				continue
			}
			if !info.exited {
				return true, nil
			}
			if snapshotIndex == 0 {
				zombies[info.handle] = struct{}{}
			} else if _, known := zombies[info.handle]; !known {
				newZombie = true
			}
		}
		if snapshotErr != nil {
			return false, fmt.Errorf("%w: process enumeration was incomplete: %w", ErrIncompleteProcessTree, snapshotErr)
		}
		if newZombie {
			return true, nil
		}
	}
	return false, nil
}
