package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"time"
)

const coordinatorParentPIDEnv = "METIS_COORDINATOR_PARENT_PID"
const coordinatorParentIdentityEnv = "METIS_COORDINATOR_PARENT_IDENTITY"

// Birth identity is a process-lifetime guard, not a credential. PID reuse must
// not allow a service child to continue after its owning service has died.
func coordinatorParentEnvironment() []string {
	pid := os.Getpid()
	identity, err := coordinatorProcessBirthIdentity(pid)
	if err != nil {
		identity = "unavailable" // Child startup fails closed on unsupported hosts.
	}
	return []string{coordinatorParentPIDEnv + "=" + strconv.Itoa(pid), coordinatorParentIdentityEnv + "=" + identity}
}

func coordinatorChildContext(parent context.Context) (context.Context, func(), error) {
	pidValue, identity := os.Getenv(coordinatorParentPIDEnv), os.Getenv(coordinatorParentIdentityEnv)
	if pidValue == "" && identity == "" {
		ctx, cancel := context.WithCancel(parent)
		return ctx, cancel, nil
	}
	pid, err := strconv.Atoi(pidValue)
	if err != nil || pid <= 1 || pid != os.Getppid() || identity == "" || identity == "unavailable" {
		return nil, nil, errors.New("coordinator worker parent identity is invalid")
	}
	return coordinatorWatchParent(parent, pid, identity, 250*time.Millisecond, coordinatorProcessBirthIdentity)
}

func coordinatorWatchParent(parent context.Context, pid int, identity string, interval time.Duration, probe func(int) (string, error)) (context.Context, func(), error) {
	observed, err := probe(pid)
	if err != nil || observed != identity {
		return nil, nil, errors.New("coordinator worker owning service is no longer alive")
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				observed, err := probe(pid)
				if err != nil || observed != identity {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(); <-done }, nil
}
