// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

// Package runtime defines limits for the Go runtime
package runtime

import (
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/DataDog/datadog-agent/pkg/util/log"

	"go.uber.org/automaxprocs/maxprocs"
)

const (
	gomaxprocsKey = "GOMAXPROCS"
	// minGOMAXPROCS is the lowest GOMAXPROCS value we allow the Go runtime to run with.
	// Go relies heavily on goroutines and a single P serializes every goroutine onto one
	// OS thread, which causes scheduling stalls (e.g. the eBPF-less system-probe event
	// pipeline cannot keep up and starts dropping events). automaxprocs floors at 1 when
	// the cgroup CPU quota is below 2 vCPUs (such as 0.25/0.5/1.0 vCPU on ECS Fargate), so
	// we enforce this minimum ourselves.
	minGOMAXPROCS = 2
)

// ensureMinProcs raises GOMAXPROCS to minGOMAXPROCS when it is currently lower.
// It never lowers an already-higher value.
func ensureMinProcs() {
	if current := runtime.GOMAXPROCS(0); current < minGOMAXPROCS {
		log.Infof("runtime: GOMAXPROCS resolved to %d, raising to the minimum of %d", current, minGOMAXPROCS)
		runtime.GOMAXPROCS(minGOMAXPROCS)
	}
}

// SetMaxProcs sets the GOMAXPROCS for the go runtime to a sane value
func SetMaxProcs() bool {

	defer func() {
		log.Infof("runtime: set GOMAXPROCS to: %d", runtime.GOMAXPROCS(0))
	}()

	var set bool
	// This call will cause GOMAXPROCS to be set to the number of vCPUs allocated to the process
	// if the process is running in a Linux environment (including when its running in a docker / K8s setup).
	_, err := maxprocs.Set(maxprocs.Logger(log.Debugf))
	if err != nil {
		log.Errorf("runtime: error auto-setting maxprocs: %v ", err)
	} else {
		set = true
	}

	// Apply the floor to the cgroup-driven (and explicit integer) path. The explicit
	// millicpu branch below applies the same floor on its own.
	ensureMinProcs()

	if max, exists := os.LookupEnv(gomaxprocsKey); exists {
		if max == "" {
			log.Errorf("runtime: GOMAXPROCS value was empty string")
			return set
		}

		_, err = strconv.Atoi(max)
		if err == nil {
			// Go runtime will already have parsed the integer and set it properly.
			return set
		}

		if before, ok := strings.CutSuffix(max, "m"); ok {
			// Value represented as millicpus.
			trimmed := before
			milliCPUs, err := strconv.Atoi(trimmed)
			if err != nil {
				log.Errorf("runtime: error parsing GOMAXPROCS milliCPUs value: %v", max)
				return set
			}

			cpus := milliCPUs / 1000
			// Floor to ensure minimal concurrency: Go relies heavily on
			// goroutines and a single OS thread can cause scheduling stalls.
			if cpus < minGOMAXPROCS {
				cpus = minGOMAXPROCS
			}
			log.Infof("runtime: GOMAXPROCS millicpu configuration: %s (resolved to %d CPUs), setting GOMAXPROCS to %d", max, milliCPUs/1000, cpus)
			runtime.GOMAXPROCS(cpus)
			set = true
			return set
		}

		log.Errorf(
			"runtime: unhandled GOMAXPROCS value: %s", max)
	}
	return set
}

// NumVCPU returns the number of virtualizes CPUs available to the process. It should be used instead of
// runtime.NumCPU() in virtualized environments like K8s to ensure that processes don't attempt to
// over-subscribe CPUs. For example, on a 16 vCPU machine in a docker container allocated 8 vCPUs,
// runtime.NumCPU() will return 16 but NumVCPU() will return 8.
func NumVCPU() int {
	// Value < 1 returns the current value without altering it.
	return runtime.GOMAXPROCS(0)
}
