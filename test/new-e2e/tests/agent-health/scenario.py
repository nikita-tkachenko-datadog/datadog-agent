# SPDX-FileCopyrightText: 2025-present Datadog, Inc. <dev@datadoghq.com>
#
# SPDX-License-Identifier: MIT
"""
Agent health demo environment and scenario definitions.

Discovered automatically by lab.demo.scenario_loader — no changes to
.dda/extend/ are needed when adding entries here.

  DEMO_ENVS  maps env-type names to DemoEnv instances, each declaring:
               - pulumi_scenario  matches RegisterScenario in scenario.go
               - create_options   CLI flags for 'dda lab demo agent-health <env> create'
               - scenarios        trigger/reset SSH actions for that env type

Issue coverage (vm env type):
  docker_file_tailing_disabled   — Docker socket permission denied
  check_execution_failure        — Custom check that always raises an exception
  invalid-config                 — Unrecognised key injected into datadog.yaml
"""

from __future__ import annotations

from lab.demo.scenarios import DemoEnv, DemoOption, Scenario, ScenarioAction

DEMO_ENVS: dict[str, DemoEnv] = {
    "vm": DemoEnv(
        pulumi_scenario="aws/agent-health-demo",
        description="AWS EC2 host with Docker and busybox containers",
        create_options=[
            DemoOption(
                name="infra_env",
                pulumi_key="ddinfra:env",
                default="aws/agent-sandbox",
                help="Pulumi infra environment name (e.g. aws/agent-sandbox)",
            ),
            DemoOption(
                name="site",
                pulumi_key="ddagent:site",
                default="datad0g.com",
                help="Datadog site to report to (e.g. datad0g.com, datadoghq.com)",
            ),
            DemoOption(
                name="agent_version",
                pulumi_key="ddagent:version",
                default=None,
                help="Explicit agent version to install, e.g. 7.57.0 (overrides --pipeline-id)",
            ),
            DemoOption(
                name="pipeline_id",
                pulumi_key="ddagent:pipeline_id",
                default=None,
                help="CI pipeline ID whose build artifacts to install",
            ),
            DemoOption(
                name="tags",
                pulumi_key="ddagent:tags",
                default=None,
                help="Comma-separated extra tags for the agent, e.g. 'env:demo,team:agent-health'",
            ),
        ],
        scenarios={
            "docker-permissions": Scenario(
                issue="docker_file_tailing_disabled",
                description="Lock down Docker socket to owner-only to trigger log tailing issue",
                trigger=ScenarioAction(
                    commands=[
                        "sudo chmod 600 /var/run/docker.sock",
                        "sudo systemctl restart datadog-agent",
                    ],
                    message="Docker socket locked to root-only — agent can no longer tail container logs.",
                ),
                reset=ScenarioAction(
                    commands=[
                        "sudo chmod 660 /var/run/docker.sock",
                        "sudo systemctl restart datadog-agent",
                    ],
                    message="Docker socket permissions restored — issue will clear on next health report.",
                ),
            ),
            "check-failure": Scenario(
                issue="check_execution_failure",
                description="Deploy a custom check that always raises an exception",
                trigger=ScenarioAction(
                    commands=[
                        "sudo mkdir -p /etc/datadog-agent/checks.d",
                        "printf 'from datadog_checks.base import AgentCheck\\nclass BrokenCheck(AgentCheck):\\n    def check(self, instance):\\n        raise RuntimeError(\"Intentional error for demo purposes\")\\n' | sudo tee /etc/datadog-agent/checks.d/broken_check.py > /dev/null",
                        "sudo mkdir -p /etc/datadog-agent/conf.d/broken_check.d",
                        "printf 'init_config:\\ninstances:\\n  - {}\\n' | sudo tee /etc/datadog-agent/conf.d/broken_check.d/conf.yaml > /dev/null",
                        "sudo systemctl restart datadog-agent",
                    ],
                    message="Broken check deployed — check_execution_failure issue will appear after the first collection cycle.",
                ),
                reset=ScenarioAction(
                    commands=[
                        "sudo rm -f /etc/datadog-agent/checks.d/broken_check.py",
                        "sudo rm -rf /etc/datadog-agent/conf.d/broken_check.d",
                        "sudo systemctl restart datadog-agent",
                    ],
                    message="Broken check removed — issue will clear after the next health report.",
                ),
            ),
            "invalid-config": Scenario(
                issue="invalid-config",
                description="Inject an unrecognised key into datadog.yaml to trigger schema validation failure",
                trigger=ScenarioAction(
                    commands=[
                        "echo 'demo_invalid_key_for_health_platform: true' | sudo tee -a /etc/datadog-agent/datadog.yaml > /dev/null",
                        "sudo systemctl restart datadog-agent",
                    ],
                    message="Invalid config key injected — invalid-config issue will appear after the agent validates its configuration.",
                ),
                reset=ScenarioAction(
                    commands=[
                        "sudo sed -i '/^demo_invalid_key_for_health_platform:/d' /etc/datadog-agent/datadog.yaml",
                        "sudo systemctl restart datadog-agent",
                    ],
                    message="Invalid config key removed — issue will clear on next validation cycle.",
                ),
            ),
        },
    ),
    # "kind": DemoEnv(
    #     pulumi_scenario="kind/agent-health-demo",
    #     description="Kind cluster for Kubernetes agent health scenarios",
    #     create_options=[],
    #     scenarios={},
    # ),
}
