// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025-present Datadog, Inc.

//go:build !linux

package cgroup

import "errors"

// GetSelfContainerID is not supported on non-Linux platforms.
func GetSelfContainerID() (string, error) {
	return "", errors.New("not supported on this platform")
}
