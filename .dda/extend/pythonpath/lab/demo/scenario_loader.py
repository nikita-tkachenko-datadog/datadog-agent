# SPDX-FileCopyrightText: 2025-present Datadog, Inc. <dev@datadoghq.com>
#
# SPDX-License-Identifier: MIT
"""
Auto-discovery and CLI generation for demo scenarios.

Scans test/new-e2e/tests/*/scenario.py and generates, for each team and
each env-type declared in DEMO_ENVS:

    dda lab demo <team> <env>          create   [--api-key] [--id] [<create_options>...]
    dda lab demo <team> <env> <name>   trigger  [--id]
                                       reset    [--id]

Teams declare DEMO_ENVS in their scenario.py:

    DEMO_ENVS = {
        "aws": DemoEnv(
            pulumi_scenario="aws/agent-health-demo",
            description="AWS EC2 + Docker",
            create_options=[DemoOption(...)],
            scenarios={"docker-permissions": Scenario(...)},
        ),
        "kind": DemoEnv(...),
    }
"""

from __future__ import annotations

import glob
import importlib.util
import json
import os
import subprocess
import sys
import types
from pathlib import Path
from typing import Any

import click
from dda.cli.base import DynamicGroup

# ---------------------------------------------------------------------------
# Repo root / module loading
# ---------------------------------------------------------------------------


_MODULE_PREFIX = "github.com/DataDog/datadog-agent"


def _find_repo_root() -> Path:
    """Walk upward until we find the go.mod that declares the agent module."""
    here = Path(__file__).resolve()
    candidate = here
    while candidate != candidate.parent:
        go_mod = candidate / "go.mod"
        if go_mod.exists() and _MODULE_PREFIX in go_mod.read_text():
            return candidate
        candidate = candidate.parent
    raise FileNotFoundError(f"Could not find repo root containing module {_MODULE_PREFIX!r}")


def _ensure_pythonpath() -> None:
    pythonpath = str(Path(__file__).resolve().parent.parent.parent)
    if pythonpath not in sys.path:
        sys.path.insert(0, pythonpath)


def _load_module(team_dir: str, repo_root: Path) -> types.ModuleType | None:
    scenario_path = repo_root / "test" / "new-e2e" / "tests" / team_dir / "scenario.py"
    if not scenario_path.exists():
        return None
    _ensure_pythonpath()
    spec = importlib.util.spec_from_file_location(f"_demolab_scenario_{team_dir}", scenario_path)
    if spec is None or spec.loader is None:
        return None
    module = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(module)  # type: ignore[union-attr]
    except Exception as exc:
        import traceback

        print(f"[demolab] Warning: failed to load {scenario_path}: {exc}", flush=True)
        traceback.print_exc()
        return None
    return module


def _discover_teams(repo_root: Path) -> list[str]:
    return sorted(
        Path(p).parent.name for p in glob.glob(str(repo_root / "test" / "new-e2e" / "tests" / "*" / "scenario.py"))
    )


# ---------------------------------------------------------------------------
# SSH helper
# ---------------------------------------------------------------------------


def _run_ssh_commands(app: Any, host_ip: str, ssh_user: str, commands: list[str]) -> None:
    for cmd in commands:
        result = subprocess.run(
            ["ssh", "-o", "StrictHostKeyChecking=no", "-o", "BatchMode=yes", f"{ssh_user}@{host_ip}", cmd],
            capture_output=True,
            text=True,
        )
        if result.returncode != 0:
            app.abort(f"Command failed on {host_ip}:\n  {cmd}\n{result.stderr}")
        if result.stdout:
            app.display_info(result.stdout.rstrip())


# ---------------------------------------------------------------------------
# Top-level demo group  →  per-team group  →  per-env group
# ---------------------------------------------------------------------------


def create_demo_group() -> DynamicGroup:
    """
    Top-level group that auto-discovers all teams with a scenario.py and
    generates a subgroup per team.
    """
    repo_root = _find_repo_root()

    class DemoGroup(DynamicGroup):
        def list_commands(self, ctx: click.Context) -> list[str]:
            return _discover_teams(repo_root)

        def get_command(self, ctx: click.Context, cmd_name: str) -> click.Command | None:
            return _make_team_group(cmd_name, repo_root)

    @click.group(cls=DemoGroup, short_help="Demo environments and scenarios for agent teams")
    @click.option("--list", "list_teams", is_flag=True, help="List available teams and exit.")
    @click.pass_context
    def demo_group(ctx: click.Context, list_teams: bool) -> None:
        """
        Demo environments and scenarios for agent teams.

        Each team with a test/new-e2e/tests/<team>/scenario.py appears as a
        subcommand. Each env-type declared in DEMO_ENVS gets its own
        create + scenario trigger/reset commands.
        """
        if list_teams:
            for team in _discover_teams(repo_root):
                click.echo(f"  {team}")
            ctx.exit()

    return demo_group


def _make_team_group(team_dir: str, repo_root: Path) -> click.Group | None:
    """Generate the per-team group containing one subgroup per env-type."""
    module = _load_module(team_dir, repo_root)
    if module is None:
        return None

    demo_envs: dict[str, Any] = getattr(module, "DEMO_ENVS", {})
    if not demo_envs:
        return None

    class TeamGroup(DynamicGroup):
        def list_commands(self, ctx: click.Context) -> list[str]:
            return list(demo_envs.keys())

        def get_command(self, ctx: click.Context, cmd_name: str) -> click.Command | None:
            demo_env = demo_envs.get(cmd_name)
            if demo_env is None:
                return None
            return _make_env_group(team_dir, cmd_name, demo_env)

    @click.group(cls=TeamGroup, short_help=f"Demo environments for {team_dir}")
    @click.option("--list", "list_envs", is_flag=True, help="List available env types and exit.")
    @click.pass_context
    def team_group(ctx: click.Context, list_envs: bool) -> None:
        """Demo environments and scenarios for the {team_dir} team."""
        if list_envs:
            for env_name, demo_env in demo_envs.items():
                click.echo(f"  {env_name}  —  {demo_env.description}")
            ctx.exit()

    team_group.__doc__ = f"Demo environments and scenarios for the {team_dir} team."
    team_group.name = team_dir
    return team_group


# ---------------------------------------------------------------------------
# Per-env group: create + scenario subcommands
# ---------------------------------------------------------------------------


def _make_env_group(team_dir: str, env_name: str, demo_env: Any) -> click.Group:
    """Generate the per-env group: create, trigger, reset."""

    class EnvGroup(DynamicGroup):
        def list_commands(self, ctx: click.Context) -> list[str]:
            cmds = ["create", "destroy"]
            if demo_env.scenarios:
                cmds += ["trigger", "reset"]
            return cmds

        def get_command(self, ctx: click.Context, cmd_name: str) -> click.Command | None:
            if cmd_name == "create":
                return _make_create_command(team_dir, env_name, demo_env)
            if cmd_name == "destroy":
                return _make_destroy_command(team_dir, env_name)
            if cmd_name in ("trigger", "reset") and demo_env.scenarios:
                return _make_action_command(cmd_name, demo_env, team_dir, env_name)
            return None

    @click.group(cls=EnvGroup, short_help=demo_env.description)
    def env_group() -> None:
        pass

    env_group.__doc__ = demo_env.description
    env_group.name = env_name
    return env_group


# ---------------------------------------------------------------------------
# create command
# ---------------------------------------------------------------------------


def _make_create_command(team_dir: str, env_name: str, demo_env: Any) -> click.Command:
    from dda.cli.base import dynamic_command, pass_app

    default_id = f"demo-{team_dir}-{env_name}"

    @pass_app
    def _create_impl(app: Any, *, api_key: str | None, id: str, **kwargs: Any) -> None:
        from lab import LabEnvironment
        from lab.config import load_config

        lab_config = load_config()

        if api_key is None:
            api_key = lab_config.get_api_key()
        if not api_key:
            app.abort("No API key provided. Use --api-key or set E2E_API_KEY.")

        repo_root = _find_repo_root()
        pulumi_dir = str(repo_root / "test" / "new-e2e" / "run")

        if LabEnvironment.exists(app, id):
            app.abort(
                f"Environment '{id}' already exists. "
                f"Run 'dda lab demo {team_dir} {env_name} destroy --id {id}' first, "
                "or use --id to choose a different name."
            )
            return

        app.display_info(f"Provisioning {team_dir}/{env_name} demo environment '{id}' ...")

        # Create the Pulumi stack if it does not already exist.  pulumi config set
        # and pulumi up both require the stack to exist first.
        init_result = subprocess.run(
            ["pulumi", "stack", "init", "--no-select", id, "-C", pulumi_dir],
            env={**os.environ},
            capture_output=True,
        )
        # Ignore "already exists" (exit 1 with that message) — any other failure is fatal.
        if init_result.returncode != 0 and b"already exists" not in (init_result.stderr or b""):
            app.abort(f"Failed to initialise Pulumi stack '{id}': {init_result.stderr.decode().strip()}")

        # Store API key as a Pulumi secret via stdin so the value is never
        # visible in process argv or CI logs.
        secret_result = subprocess.run(
            ["pulumi", "config", "set", "--secret", "ddagent:apiKey", "-s", id, "-C", pulumi_dir],
            input=api_key,
            env={**os.environ},
            capture_output=True,
            text=True,
        )
        if secret_result.returncode != 0:
            app.abort("Failed to set API key as Pulumi secret.")

        config_args = [
            "-c",
            f"scenario={demo_env.pulumi_scenario}",
            "-c",
            f"demolab:teamDir={team_dir}",
            "-c",
            f"demolab:envName={env_name}",
        ]

        # Forward AWS infra config from ~/.test_infra_config.yaml so the provisioner
        # can find the EC2 key pair and SSH keys without manual pulumi config set calls.
        aws = lab_config.aws
        if aws.key_pair_name:
            config_args += ["-c", f"ddinfra:aws/defaultKeyPairName={aws.key_pair_name}"]
        if aws.public_key_path:
            config_args += ["-c", f"ddinfra:aws/defaultPublicKeyPath={aws.public_key_path}"]
        if aws.private_key_path:
            config_args += ["-c", f"ddinfra:aws/defaultPrivateKeyPath={aws.private_key_path}"]
        if aws.private_key_password:
            config_args += ["-c", f"ddinfra:aws/defaultPrivateKeyPassword={aws.private_key_password}"]

        for opt in demo_env.create_options:
            value = kwargs.get(opt.name)
            if value is not None:
                config_args += ["-c", f"{opt.pulumi_key}={value}"]

        # Save a minimal record before pulumi up so the stack is trackable
        # even if provisioning fails partway through.
        site = kwargs.get("site", "")
        metadata: dict[str, Any] = {
            "ssh_user": "ubuntu",
            "stack": id,
            "pulumi_dir": pulumi_dir,
            "demo_env": env_name,
        }
        if site:
            metadata["site"] = site
        LabEnvironment(app, name=id, env_type=team_dir, category="demo", metadata=metadata).save()

        result = subprocess.run(
            ["pulumi", "up", "--yes", "-s", id, "-C", pulumi_dir] + config_args,
            env={**os.environ},
        )
        if result.returncode != 0:
            app.abort(
                f"pulumi up failed (exit {result.returncode}). "
                f"Run 'dda lab demo {team_dir} {env_name} destroy --id {id}' to clean up."
            )

        # Read the stable hostIP output exported by the provisioner.
        out = subprocess.run(
            ["pulumi", "stack", "output", "--json", "-s", id, "-C", pulumi_dir],
            capture_output=True,
            text=True,
        )
        host_ip = None
        if out.returncode == 0 and out.stdout.strip():
            try:
                outputs = json.loads(out.stdout)
                host_ip = outputs.get("hostIP")
                if not host_ip and demo_env.host_required:
                    app.display_warning(
                        "Stack output 'hostIP' is absent — check that the provisioner "
                        "calls ctx.Export(\"hostIP\", ...). trigger/reset will not work."
                    )
            except (json.JSONDecodeError, AttributeError):
                if demo_env.host_required:
                    app.display_warning("Could not parse stack outputs; trigger/reset may not work.")
        elif out.returncode == 0 and not out.stdout.strip() and demo_env.host_required:
            app.display_warning("pulumi stack output returned no data; hostIP may not be set.")

        if host_ip:
            metadata["host_ip"] = host_ip
        LabEnvironment(app, name=id, env_type=team_dir, category="demo", metadata=metadata).save()

        app.display_success(f"Environment '{id}' created.")
        if host_ip:
            app.display_info(f"  Host IP : {host_ip}")
            app.display_info(f"  SSH     : ssh ubuntu@{host_ip}")
            if site:
                app.display_info(f"  Datadog : https://app.{site}")
        else:
            app.display_info("  (Could not read host IP from stack outputs)")

    fn = _create_impl
    fn = click.option("--id", "-i", default=default_id, show_default=True, help="Environment id")(fn)
    fn = click.option("--api-key", default=None, envvar="E2E_API_KEY", help="Datadog API key")(fn)
    for opt in reversed(demo_env.create_options):
        cli_name = f"--{opt.name.replace('_', '-')}"
        fn = click.option(
            cli_name,
            default=opt.default,
            required=opt.required,
            show_default=opt.show_default and opt.default is not None,
            help=opt.help,
        )(fn)

    fn = dynamic_command(short_help=f"Provision a {team_dir} {env_name} demo environment")(fn)
    fn.__doc__ = (
        f"Provision a {team_dir} demo environment using {env_name}.\n\n"
        "The API key is resolved from --api-key, E2E_API_KEY, or ~/.test_infra_config.yaml."
    )
    fn.name = "create"
    return fn


# ---------------------------------------------------------------------------
# destroy command
# ---------------------------------------------------------------------------


def _make_destroy_command(team_dir: str, env_name: str) -> click.Command:
    from dda.cli.base import dynamic_command, pass_app

    default_id = f"demo-{team_dir}-{env_name}"

    @dynamic_command(short_help=f"Destroy the {team_dir} {env_name} demo environment")
    @click.option("--id", "-i", default=default_id, show_default=True, help="Environment id")
    @click.option("--yes", "-y", is_flag=True, help="Skip confirmation prompt.")
    @pass_app
    def cmd(app: Any, *, id: str, yes: bool) -> None:
        """Destroy the Pulumi stack and remove the saved environment record."""
        from lab import LabEnvironment

        env = LabEnvironment.load(app, id)
        if env is None:
            app.abort(f"Environment '{id}' not found.")

        if not yes and not click.confirm(f"Destroy {team_dir}/{env_name} environment '{id}'?"):
            app.display_info("Aborting.")
            return

        stack = env.metadata.get("stack", id)
        # Read pulumi_dir from metadata so the correct Pulumi program is used
        # even if it differs from the standard location.
        pulumi_dir = env.metadata.get("pulumi_dir") or str(_find_repo_root() / "test" / "new-e2e" / "run")

        app.display_info(f"Destroying Pulumi stack '{stack}' ...")
        result = subprocess.run(
            ["pulumi", "destroy", "--yes", "-s", stack, "-C", pulumi_dir],
            env={**os.environ},
        )
        if result.returncode != 0:
            app.abort(
                f"pulumi destroy failed (exit {result.returncode}). "
                f"Local record kept — re-run 'dda lab demo {team_dir} {env_name} destroy --id {id}' after fixing credentials."
            )
            return

        env.delete()
        app.display_success(f"Environment '{id}' destroyed.")

    cmd.name = "destroy"
    return cmd


# ---------------------------------------------------------------------------
# trigger / reset commands with --scenario selection
# ---------------------------------------------------------------------------


def _make_action_command(action: str, demo_env: Any, team_dir: str, env_name: str) -> click.Command:
    from dda.cli.base import dynamic_command, pass_app

    scenario_names = sorted(demo_env.scenarios.keys())
    default_scenario = scenario_names[0] if len(scenario_names) == 1 else None
    scenarios_help = ", ".join(scenario_names)
    short_help = "Trigger a scenario issue" if action == "trigger" else "Reset a scenario to healthy state"

    @dynamic_command(short_help=short_help)
    @click.option("--scenario", "-s", default=default_scenario, help=f"Scenario to run. Available: {scenarios_help}")
    @click.option("--list", "list_scenarios", is_flag=True, help="List available scenarios and exit.")
    @click.option("--id", "-i", default=None, help=f"Environment id (default: first {team_dir}/{env_name} env)")
    @pass_app
    def cmd(app: Any, *, scenario: str | None, list_scenarios: bool, id: str | None) -> None:
        if list_scenarios:
            for name, sc in demo_env.scenarios.items():
                app.display_info(f"  {name}  —  {sc.description}")
            return

        if not scenario:
            app.abort(f"--scenario/-s is required. Available: {scenarios_help}")

        sc = demo_env.scenarios.get(scenario)
        if sc is None:
            app.abort(f"Unknown scenario '{scenario}'. Available: {scenarios_help}")

        _run_action(app, id, sc, team_dir, env_name, action)

    cmd.name = action
    return cmd


def _run_action(app: Any, env_id: str | None, scenario: Any, team_dir: str, env_name: str, action: str) -> None:
    from lab import LabEnvironment

    if env_id is None:
        envs = [e for e in LabEnvironment.load_all(app, env_type=team_dir) if e.metadata.get("demo_env") == env_name]
        if not envs:
            app.abort(
                f"No {team_dir}/{env_name} demo environments found. "
                f"Run 'dda lab demo {team_dir} {env_name} create' first."
            )
        # Sort by created_at descending so the most recent environment is preferred.
        envs.sort(key=lambda e: e.created_at, reverse=True)
        if len(envs) > 1:
            app.display_warning(
                f"Multiple {team_dir}/{env_name} environments found; using the most recent ('{envs[0].name}'). "
                f"Use --id to select a specific one."
            )
        env = envs[0]
    else:
        env = LabEnvironment.load(app, env_id)
        if env is None:
            app.abort(f"Environment '{env_id}' not found.")
            return

    host_ip = env.metadata.get("host_ip")
    ssh_user = env.metadata.get("ssh_user", "ubuntu")
    if not host_ip:
        app.abort(f"Environment '{env.name}' has no host_ip in metadata.")

    act = scenario.trigger if action == "trigger" else scenario.reset
    app.display_info(f"Running '{action}' for issue '{scenario.issue}' on {host_ip}...")
    _run_ssh_commands(app, host_ip, ssh_user, act.commands)
    app.display_success(act.message)
