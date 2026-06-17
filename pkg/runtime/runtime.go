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
	minGOMAXPROCS = 2
)

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
	_, err := maxprocs.Set(maxprocs.Logger(log.Debugf))
	if err != nil {
		log.Errorf("runtime: error auto-setting maxprocs: %v ", err)
	} else {
		set = true
	}

	if max, exists := os.LookupEnv(gomaxprocsKey); exists {
		switch {
		case max == "":
			log.Errorf("runtime: GOMAXPROCS value was empty string")
		case isInteger(max):
		case strings.HasSuffix(max, "m"):
			milliCPUs, err := strconv.Atoi(strings.TrimSuffix(max, "m"))
			if err != nil {
				log.Errorf("runtime: error parsing GOMAXPROCS milliCPUs value: %v", max)
			} else {
				cpus := milliCPUs / 1000
				log.Infof("runtime: GOMAXPROCS millicpu configuration: %s (resolved to %d CPUs)", max, cpus)
				runtime.GOMAXPROCS(cpus)
				set = true
			}
		default:
			log.Errorf("runtime: unhandled GOMAXPROCS value: %s", max)
		}
	}

	ensureMinProcs()

	return set
}

func isInteger(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

// NumVCPU returns the number of virtualizes CPUs available to the process. It should be used instead of
// runtime.NumCPU() in virtualized environments like K8s to ensure that processes don't attempt to
// over-subscribe CPUs. For example, on a 16 vCPU machine in a docker container allocated 8 vCPUs,
// runtime.NumCPU() will return 16 but NumVCPU() will return 8.
func NumVCPU() int {
	// Value < 1 returns the current value without altering it.
	return runtime.GOMAXPROCS(0)
}
