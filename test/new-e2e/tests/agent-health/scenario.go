// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025-present Datadog, Inc.

package agenthealth

import (
	"github.com/DataDog/datadog-agent/test/e2e-framework/registry"
	"github.com/DataDog/datadog-agent/test/e2e-framework/testing/components"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func init() {
	registry.RegisterScenario("aws/agent-health-demo", agentHealthDemoRun)
}

// agentHealthDemoRun provisions an agent-health demo environment against real Datadog intake.
//
// Pulumi config keys (namespace "ddagent", all read by aws.NewEnvironment):
//   - apiKey       — Datadog API key (required, secret)
//   - site         — Datadog site, e.g. "datad0g.com" (optional, injected by provisioner)
//   - pipelineId   — Agent CI pipeline ID (optional)
//   - agentVersion — Explicit agent version, e.g. "7.57.0" (optional, overrides pipelineId)
func agentHealthDemoRun(ctx *pulumi.Context) error {
	var env dockerPermissionEnv
	env.RemoteHost = &components.RemoteHost{}
	env.Agent = &components.RemoteHostAgent{}
	env.Docker = &components.RemoteHostDocker{}
	// env.Fakeintake intentionally omitted — demo reports to real Datadog.

	return dockerPermissionEnvProvisioner(withoutFakeintake())(ctx, &env)
}
