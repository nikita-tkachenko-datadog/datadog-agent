// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025-present Datadog, Inc.

package agenthealth

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DataDog/agent-payload/v5/healthplatform"

	"github.com/DataDog/datadog-agent/test/e2e-framework/components/datadog/agentparams"
	"github.com/DataDog/datadog-agent/test/e2e-framework/scenarios/aws/ec2"
	"github.com/DataDog/datadog-agent/test/e2e-framework/testing/e2e"
	"github.com/DataDog/datadog-agent/test/e2e-framework/testing/environments"
	awshost "github.com/DataDog/datadog-agent/test/e2e-framework/testing/provisioners/aws/host"
)

type networkProbeSuite struct {
	e2e.BaseSuite[environments.Host]
}

func TestNetworkProbeSuite(t *testing.T) {
	t.Parallel()
	e2e.Run(t, &networkProbeSuite{},
		e2e.WithProvisioner(awshost.Provisioner(
			awshost.WithRunOptions(
				ec2.WithAgentOptions(
					agentparams.WithAgentConfig(healthPlatformAgentConfig),
				),
			),
		)),
	)
}

// TestNetworkProbeInitFailureLifecycle verifies that when NPM is enabled but the
// eBPF network tracer fails to initialize (simulated via kernel version exclusion),
// system-probe self-reports a network-probe-init-failure issue in fakeintake as
// NEW, and that removing the exclusion (successful tracer init) causes the issue
// to transition to RESOLVED.
//
// Cross-restart persistence is tested separately in TestResilienceSuite.
func (suite *networkProbeSuite) TestNetworkProbeInitFailureLifecycle() {
	fakeIntake := suite.Env().FakeIntake.Client()

	// Get the running kernel version to use as an exclusion list entry.
	// system-probe's IsTracerSupportedByOS compares ParseVersion(entry) against
	// HostVersion() (from vDSO). On distribution kernels the vDSO LINUX_VERSION_CODE
	// may use the ABI/build number as the patch, so we exclude the entire major.minor
	// range (patch 0-255) to reliably trigger the exclusion regardless of the exact
	// kernel build the E2E image is running.
	unameR := strings.TrimSpace(suite.Env().RemoteHost.MustExecute("uname -r"))
	var major, minor, patch int
	_, _ = fmt.Sscanf(unameR, "%d.%d.%d", &major, &minor, &patch)
	kernelVerExclude := fmt.Sprintf("%d.%d.%d", major, minor, patch)

	// Build a YAML exclusion list covering all possible patch values for major.minor
	// so the exclusion fires even when vDSO encodes the ABI in the sublevel.
	var exclusionYAML strings.Builder
	for p := 0; p <= 255; p++ {
		fmt.Fprintf(&exclusionYAML, "    - %d.%d.%d\n", major, minor, p)
	}

	const issueID = "network-probe-init-failure"

	suite.T().Run("IssueDetection", func(t *testing.T) {
		// Enable NPM and add the running kernel to the exclusion list so that
		// IsTracerSupportedByOS returns an error and system-probe calls
		// reportNetworkProbeInitFailure.
		suite.UpdateEnv(awshost.Provisioner(
			awshost.WithRunOptions(
				ec2.WithAgentOptions(
					agentparams.WithAgentConfig(healthPlatformAgentConfig),
					agentparams.WithSystemProbeConfig(
						"network_config:\n  enabled: true\nsystem_probe_config:\n  excluded_linux_versions:\n"+exclusionYAML.String(),
					),
				),
			),
		))

		// datadog-agent-sysprobe is auto-started by systemd as a dependency of datadog-agent
		// when network_config.enabled is set. The auto-started instance reads the new
		// system-probe.yaml (written before the agent restart by UpdateEnv), so no explicit
		// restart is needed. An explicit restart would cause the issue to be reported a second
		// time as ONGOING (not NEW), causing the IssueState_ISSUE_STATE_NEW check to miss it.
		t.Logf("kernel exclusion string: %q (uname -r was %q)", kernelVerExclude, unameR)

		// Log system-probe AND agent logs for diagnostics if the test times out.
		defer func() {
			if t.Failed() {
				status, _ := suite.Env().RemoteHost.Execute("sudo systemctl status datadog-agent-sysprobe --no-pager -l")
				t.Logf("system-probe status:\n%s", status)
				logs, _ := suite.Env().RemoteHost.Execute("sudo journalctl -u datadog-agent-sysprobe -n 100 --no-pager 2>&1 || sudo cat /var/log/datadog/system-probe.log 2>&1 | tail -100")
				t.Logf("system-probe logs:\n%s", logs)
				agentLogs, _ := suite.Env().RemoteHost.Execute("sudo journalctl -u datadog-agent -n 50 --no-pager 2>&1 | grep -i 'health.platform\\|health_platform\\|egress' || sudo grep -i 'health.platform\\|health_platform\\|egress' /var/log/datadog/agent.log 2>&1 | tail -50")
				t.Logf("agent health platform logs:\n%s", agentLogs)
			}
		}()

		var issues []*healthplatform.Issue
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			payloads, err := fakeIntake.GetAgentHealth()
			assert.NoError(ct, err)
			issues = nil
			for _, p := range payloads {
				for _, iss := range findIssuesByID(t, p, issueID) {
					if iss.PersistedIssue != nil && iss.PersistedIssue.State == healthplatform.IssueState_ISSUE_STATE_NEW {
						issues = append(issues, iss)
					}
				}
			}
			assert.NotEmpty(ct, issues, "network-probe-init-failure issue not found as NEW in fakeintake")
		}, defaultIssueTimeout, defaultIssuePollInterval, "network-probe-init-failure issue not detected in fakeintake")

		require.NotEmpty(t, issues)
		issue := issues[0]
		assert.Equal(t, "network_probe_init_failure", issue.IssueName)
		assert.Equal(t, "runtime", issue.Category)
		assert.Equal(t, "system-probe", issue.Source)
		assert.Equal(t, "system-probe", issue.Location)
		assert.Contains(t, issue.Tags, "npm")
		require.NotNil(t, issue.Remediation)
		assert.NotEmpty(t, issue.Remediation.Summary)
		require.NotNil(t, issue.Extra)
		errorVal := issue.Extra.GetFields()["error"]
		require.NotNil(t, errorVal, "extra must contain an 'error' field")
		assert.NotEmpty(t, errorVal.GetStringValue(), "extra.error must be non-empty")
		npmEnabledVal := issue.Extra.GetFields()["npm_enabled"]
		require.NotNil(t, npmEnabledVal, "extra must contain an 'npm_enabled' field")
		assert.Equal(t, "true", npmEnabledVal.GetStringValue())
	})

	suite.T().Run("Resolution", func(t *testing.T) {
		// Remove the kernel exclusion while keeping NPM enabled so the tracer can
		// initialize successfully and call resolveNetworkProbeInitFailure.
		suite.UpdateEnv(awshost.Provisioner(
			awshost.WithRunOptions(
				ec2.WithAgentOptions(
					agentparams.WithAgentConfig(healthPlatformAgentConfig),
					agentparams.WithSystemProbeConfig("network_config:\n  enabled: true\n"),
				),
			),
		))

		// datadog-agent-sysprobe is auto-restarted by systemd when the agent restarts;
		// since network_config.enabled is still set, the tracer now initializes
		// successfully (no kernel exclusion) and calls resolveNetworkProbeInitFailure.
		require.NoError(t, fakeIntake.FlushServerAndResetAggregators())

		// Resolved issues are removed from the active-issues map, so the egress skips
		// sending payloads (count=0) after resolution. The absence of any active-issue
		// payload over the window is the resolution signal.
		require.Never(t, func() bool {
			payloads, _ := fakeIntake.GetAgentHealth()
			for _, p := range payloads {
				if len(findIssuesByID(t, p, issueID)) > 0 {
					return true
				}
			}
			return false
		}, defaultIssueAbsenceWindow, defaultIssuePollInterval,
			"network-probe-init-failure issue reappeared after fix")
	})
}
