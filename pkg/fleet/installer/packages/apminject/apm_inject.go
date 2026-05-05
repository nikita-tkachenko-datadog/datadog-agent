// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build !windows

// Package apminject implements the apm injector installer
package apminject

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"go.uber.org/multierr"
	"go.yaml.in/yaml/v2"

	"github.com/DataDog/datadog-agent/pkg/fleet/installer/env"
	"github.com/DataDog/datadog-agent/pkg/fleet/installer/packages/embedded"
	"github.com/DataDog/datadog-agent/pkg/fleet/installer/packages/service/systemd"
	"github.com/DataDog/datadog-agent/pkg/fleet/installer/setup/config"
	"github.com/DataDog/datadog-agent/pkg/fleet/installer/telemetry"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

const (
	injectorPath          = "/opt/datadog-packages/datadog-apm-inject/stable"
	ldSoPreloadPath       = "/etc/ld.so.preload"
	oldLauncherPath       = "/opt/datadog/apm/inject/launcher.preload.so"
	localStableConfigPath = "/etc/datadog-agent/application_monitoring.yaml"

	// defaultTmpfsInjectDir is a symlink living on tmpfs (/run is a tmpfs on
	// systemd hosts, wiped on every boot) that points at the persistent
	// injector payload under injectorPath. When systemd manages the injector,
	// /etc/ld.so.preload references the launcher through this symlink instead
	// of its persistent path. After a reboot the symlink is gone, so a stale
	// ld.so.preload entry resolves to a missing file and ld.so silently skips
	// it — the host comes up usable even if shutdown-time cleanup never ran.
	// The datadog-apm-inject service recreates the symlink on every boot.
	//
	// A symlink (not a copy) is required: the AppArmor profile only grants load
	// permission to /opt/datadog-packages/** and AppArmor mediates the resolved
	// path, so the launcher file must stay under injectorPath.
	defaultTmpfsInjectDir = "/run/datadog-apm-inject"
)

// NewInstaller returns a new APM injector installer
func NewInstaller() *InjectorInstaller {
	a := &InjectorInstaller{
		installPath:    injectorPath,
		tmpfsInjectDir: defaultTmpfsInjectDir,
		Env:            env.FromEnv(),
	}
	a.ldPreloadFileInstrument = newFileMutator(ldSoPreloadPath, a.setLDPreloadConfigContent, nil, nil)
	a.ldPreloadFileUninstrument = newFileMutator(ldSoPreloadPath, a.deleteLDPreloadConfigContent, nil, nil)
	a.dockerConfigInstrument = newFileMutator(dockerDaemonPath, a.setDockerConfigContent, nil, nil)
	a.dockerConfigUninstrument = newFileMutator(dockerDaemonPath, a.deleteDockerConfigContent, nil, nil)
	return a
}

// InjectorInstaller installs the APM injector
type InjectorInstaller struct {
	installPath               string
	ldPreloadFileInstrument   *fileMutator
	ldPreloadFileUninstrument *fileMutator
	dockerConfigInstrument    *fileMutator
	dockerConfigUninstrument  *fileMutator
	Env                       *env.Env

	// tmpfsInjectDir is the tmpfs symlink directory used to reference the
	// launcher in a reboot-safe way. Defaults to defaultTmpfsInjectDir;
	// overridable in tests.
	tmpfsInjectDir string
	// useTmpfsLink, when set, makes InstrumentLDPreload reference the launcher
	// through the tmpfs symlink (see InstrumentLDPreloadService) rather than
	// its persistent path. Set on systemd-managed hosts only.
	useTmpfsLink bool
	// launcherPath is the path written to /etc/ld.so.preload. Empty until
	// resolved; ldPreloadEntry falls back to the persistent OCI path.
	launcherPath string

	rollbacks []func() error
	cleanups  []func()
}

// Finish cleans up the APM injector
// Runs rollbacks if an error is passed and always runs cleanups
func (a *InjectorInstaller) Finish(err error) {
	if err != nil {
		// Run rollbacks in reverse order
		for i := len(a.rollbacks) - 1; i >= 0; i-- {
			if a.rollbacks[i] == nil {
				continue
			}
			if rollbackErr := a.rollbacks[i](); rollbackErr != nil {
				log.Warnf("rollback failed: %v", rollbackErr)
			}
		}
	}

	// Run cleanups in reverse order
	for i := len(a.cleanups) - 1; i >= 0; i-- {
		if a.cleanups[i] == nil {
			continue
		}
		a.cleanups[i]()
	}
}

// Setup sets up the APM injector
func (a *InjectorInstaller) Setup(ctx context.Context) error {
	var err error

	if err = setupAppArmor(ctx); err != nil {
		return err
	}

	// Create mandatory dirs
	err = os.MkdirAll("/var/log/datadog/dotnet", 0755)
	if err != nil && !os.IsExist(err) {
		return fmt.Errorf("error creating /var/log/datadog/dotnet: %w", err)
	}
	// a umask 0022 is frequently set by default, so we need to change the permissions by hand
	err = os.Chmod("/var/log/datadog/dotnet", 0777)
	if err != nil {
		return fmt.Errorf("error changing permissions on /var/log/datadog/dotnet: %w", err)
	}
	err = os.Mkdir("/etc/datadog-agent/inject", 0755)
	if err != nil && !os.IsExist(err) {
		return fmt.Errorf("error creating /etc/datadog-agent/inject: %w", err)
	}

	err = a.addLocalStableConfig(ctx)
	if err != nil {
		return fmt.Errorf("error adding stable config file: %w", err)
	}

	err = a.addInstrumentScripts(ctx)
	if err != nil {
		return fmt.Errorf("error adding install scripts: %w", err)
	}

	return a.Instrument(ctx)
}

// Remove removes the APM injector
func (a *InjectorInstaller) Remove(ctx context.Context) (err error) {
	span, _ := telemetry.StartSpanFromContext(ctx, "remove_injector")
	defer func() { span.Finish(err) }()

	err = a.removeInstrumentScripts(ctx)
	if err != nil {
		return fmt.Errorf("error removing install scripts: %w", err)
	}

	err = removeAppArmor(ctx)
	if err != nil {
		return fmt.Errorf("error removing AppArmor profile: %w", err)
	}

	return a.Uninstrument(ctx)
}

// Instrument instruments the APM injector
func (a *InjectorInstaller) Instrument(ctx context.Context) (retErr error) {
	if shouldInstrumentHost(a.Env) {
		systemdRunning, err := systemd.IsRunning()
		if err != nil {
			return err
		}
		if systemdRunning {
			// Best-effort: set up the systemd unit that re-asserts
			// /etc/ld.so.preload on every boot. This never fails the install — the
			// unit is a reliability enhancement, and the direct InstrumentLDPreload
			// below already persists across reboots on its own. When the unit is
			// managed, the direct write below must reference the launcher through
			// the reboot-safe tmpfs symlink (just like the service does on boot):
			// otherwise it would append the persistent /opt path next to the
			// service-written /run entry and reintroduce the reboot hazard the
			// tmpfs path exists to avoid.
			if managed := a.setupSystemdPreloadUnit(ctx); managed {
				a.useTmpfsLink = true
			}
		}
		// Always write /etc/ld.so.preload directly so the current boot is covered
		// (and so host injection works even when the systemd unit was skipped, or
		// failed to start immediately). useTmpfsLink, set above, selects the
		// tmpfs symlink path on systemd-managed hosts and the persistent path
		// otherwise; either way this write is idempotent with what the service does.
		if err := a.InstrumentLDPreload(ctx); err != nil {
			return err
		}
	}

	dockerIsInstalled := isDockerInstalled(ctx)
	if mustInstrumentDocker(a.Env) && !dockerIsInstalled {
		return errors.New("DD_APM_INSTRUMENTATION_ENABLED is set to docker but docker is not installed")
	}
	if shouldInstrumentDocker(a.Env) && dockerIsInstalled {
		// Set up defaults for agent sockets -- requires an agent restart
		if err := a.configureSocketsEnv(ctx); err != nil {
			return err
		}
		a.cleanups = append(a.cleanups, a.dockerConfigInstrument.cleanup)
		rollbackDocker, err := a.instrumentDocker(ctx)
		if err != nil {
			return err
		}
		a.rollbacks = append(a.rollbacks, rollbackDocker)

		// Verify that the docker runtime is as expected
		if err := a.verifyDockerRuntime(ctx); err != nil {
			return err
		}
	}

	return nil
}

// setupSystemdPreloadUnit installs (or refreshes) the datadog-apm-inject systemd
// unit that re-asserts /etc/ld.so.preload on every boot, when a datadog-installer
// supporting `apm instrument-start` is available. If none is available, or the
// unit setup fails for any reason, it degrades to direct ld.so.preload management
// (the InstrumentLDPreload call in Instrument): the unit is a reliability
// enhancement and must never fail the package install. Any stale unit left by a
// previous install is removed so a doomed ExecStart is not left enabled.
//
// It returns true when the unit was successfully set up (systemd-managed mode),
// so the caller references the launcher through the reboot-safe tmpfs symlink in
// its own direct ld.so.preload write. A false return means the caller must use
// the persistent path (no unit recreates the tmpfs symlink on boot).
func (a *InjectorInstaller) setupSystemdPreloadUnit(ctx context.Context) (managed bool) {
	span, ctx := telemetry.StartSpanFromContext(ctx, "setup_systemd_preload_unit")
	defer func() { span.Finish(nil) }()

	mgr := NewSystemdServiceManager()
	installerPath := mgr.InstallerPath()
	span.SetTag("installer_path", installerPath)

	if installerPath == "" {
		// No installer on disk supports `apm instrument-start` (no candidate at
		// all, or only older ones — e.g. the pinned agent in the DJM/Databricks
		// flow, or a stale `stable` symlink on upgrade). Skip the unit and rely on
		// the direct /etc/ld.so.preload write in Instrument, removing any stale
		// unit a previous install left behind.
		span.SetTag("mode", "direct_fallback")
		if err := mgr.Uninstall(ctx); err != nil {
			log.Warnf("failed to remove stale apm-inject systemd service: %v", err)
		}
		return false
	}

	span.SetTag("mode", "systemd")
	if err := mgr.Setup(ctx); err != nil {
		// Degrade rather than abort: clean up any partial unit and rely on the
		// direct /etc/ld.so.preload write in Instrument.
		span.SetTag("mode", "direct_fallback_after_setup_error")
		span.SetTag("setup_error", err.Error())
		log.Warnf("failed to set up apm-inject systemd service, using direct /etc/ld.so.preload: %v", err)
		if uErr := mgr.Uninstall(ctx); uErr != nil {
			log.Warnf("failed to clean up partial apm-inject systemd service: %v", uErr)
		}
		return false
	}
	a.rollbacks = append(a.rollbacks, func() error {
		return mgr.Uninstall(ctx)
	})
	return true
}

// Uninstrument uninstruments the APM injector
func (a *InjectorInstaller) Uninstrument(ctx context.Context) error {
	errs := []error{}

	if shouldInstrumentHost(a.Env) {
		systemdRunning, err := systemd.IsRunning()
		if err != nil {
			errs = append(errs, err)
		} else if systemdRunning {
			errs = append(errs, NewSystemdServiceManager().Uninstall(ctx))
			// Safety net: explicitly remove the ld.so.preload entry even if the
			// service's ExecStop did not run (e.g. service was in a failed state
			// when stopped). UninstrumentLDPreload is pure file I/O and idempotent.
			errs = append(errs, a.UninstrumentLDPreload(ctx))
		} else {
			errs = append(errs, a.UninstrumentLDPreload(ctx))
		}
	}

	if shouldInstrumentDocker(a.Env) {
		dockerErr := a.uninstrumentDocker(ctx)
		errs = append(errs, dockerErr)
	}

	return multierr.Combine(errs...)
}

// setLDPreloadConfigContent sets the content of the LD preload configuration
func (a *InjectorInstaller) setLDPreloadConfigContent(_ context.Context, ldSoPreload []byte) ([]byte, error) {
	launcherPreloadPath := a.ldPreloadEntry()

	if strings.Contains(string(ldSoPreload), launcherPreloadPath) {
		// If the line of interest is already in /etc/ld.so.preload, return fast
		return ldSoPreload, nil
	}

	// Migrate any previously-written launcher path to the active one, in place.
	// This covers the legacy deb path and — on hosts instrumented before the
	// switch to the tmpfs symlink — the persistent OCI path. Leaving a stale
	// persistent entry behind would re-introduce the reboot hazard the tmpfs
	// path exists to avoid.
	out := ldSoPreload
	migrated := false
	for _, legacy := range []string{oldLauncherPath, path.Join(a.installPath, "inject", "launcher.preload.so")} {
		if legacy == launcherPreloadPath {
			continue
		}
		if bytes.Contains(out, []byte(legacy)) {
			out = bytes.ReplaceAll(out, []byte(legacy), []byte(launcherPreloadPath))
			migrated = true
		}
	}
	if migrated {
		return out, nil
	}

	var buf bytes.Buffer
	buf.Write(ldSoPreload)
	// Append the launcher preload path to the file
	if len(ldSoPreload) > 0 && ldSoPreload[len(ldSoPreload)-1] != '\n' {
		buf.WriteByte('\n')
	}
	buf.WriteString(launcherPreloadPath)
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// ldPreloadEntry returns the launcher path written to /etc/ld.so.preload. When
// resolved (systemd-managed hosts), this is the tmpfs symlink path; otherwise
// it falls back to the persistent launcher under installPath.
func (a *InjectorInstaller) ldPreloadEntry() string {
	if a.launcherPath != "" {
		return a.launcherPath
	}
	return path.Join(a.installPath, "inject", "launcher.preload.so")
}

// enableTmpfsLink (re)creates the tmpfs symlink pointing at the persistent
// injector payload and switches the ld.so.preload entry to the tmpfs path. It
// must only be called after the real launcher has been verified, so a broken
// launcher is never reachable through the symlink. On rollback the symlink is
// removed.
func (a *InjectorInstaller) enableTmpfsLink() error {
	target := path.Join(a.installPath, "inject")
	if err := linkAtomically(target, a.tmpfsInjectDir); err != nil {
		return fmt.Errorf("failed to create tmpfs injector symlink %s -> %s: %w", a.tmpfsInjectDir, target, err)
	}
	a.launcherPath = path.Join(a.tmpfsInjectDir, "launcher.preload.so")
	a.rollbacks = append(a.rollbacks, func() error {
		return os.Remove(a.tmpfsInjectDir)
	})
	return nil
}

// linkAtomically creates (or atomically replaces) a symlink at link pointing
// to target.
func linkAtomically(target, link string) error {
	if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
		return err
	}
	tmp := link + ".tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// deleteLDPreloadConfigContent deletes the content of the LD preload configuration
func (a *InjectorInstaller) deleteLDPreloadConfigContent(_ context.Context, ldSoPreload []byte) ([]byte, error) {
	// Match both the persistent OCI install path and the tmpfs symlink path, so
	// the entry is removed regardless of which one a host was instrumented
	// with. The optional dynamic subdir supports no-op 32bit libraries.
	alts := []string{regexp.QuoteMeta(a.installPath) + "/inject"}
	if a.tmpfsInjectDir != "" {
		alts = append(alts, regexp.QuoteMeta(a.tmpfsInjectDir))
	}
	regexPath := "(" + strings.Join(alts, "|") + ")/(.*?/)?launcher\\.preload\\.so"
	// match beginning of the line and the [dynamic] path and trailing whitespaces (spaces\tabs\new lines) OR
	// match ANY leading whitespaces (spaces\tabs\new lines) with the dynamic path
	matcher := regexp.MustCompile("^" + regexPath + "(\\s*)|(\\s*)" + regexPath)
	return []byte(matcher.ReplaceAllString(string(ldSoPreload), "")), nil
}

func (a *InjectorInstaller) verifySharedLib(ctx context.Context, libPath string) (err error) {
	span, _ := telemetry.StartSpanFromContext(ctx, "verify_shared_lib")
	defer func() { span.Finish(err) }()

	if _, err := os.Stat(libPath); os.IsNotExist(err) {
		return fmt.Errorf("launcher library not found at %s", libPath)
	}

	echoPath, err := exec.LookPath("echo")
	if err != nil {
		// If echo is not found, to not block install,
		// we skip the test and add it to the span.
		span.SetTag("skipped", true)
		return nil
	}
	cmd := exec.Command(echoPath, "1")
	cmd.Env = append(os.Environ(), "LD_PRELOAD="+libPath)
	var buf bytes.Buffer
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to verify injected lib %s (%w): %s", libPath, err, buf.String())
	}
	return nil
}

// InstrumentLDPreloadService adds the injector to /etc/ld.so.preload using the
// reboot-safe tmpfs symlink path. It is the entry point used when systemd
// manages the injector: both during install (Instrument) and on every boot via
// the datadog-apm-inject service's "instrument-start host" command. Referencing
// the launcher through a /run symlink that the service recreates each boot means
// a stale ld.so.preload entry becomes inert after a reboot, so the host stays
// usable even if shutdown-time cleanup did not run.
func (a *InjectorInstaller) InstrumentLDPreloadService(ctx context.Context) error {
	a.useTmpfsLink = true
	return a.InstrumentLDPreload(ctx)
}

// InstrumentLDPreload directly adds the injector library to /etc/ld.so.preload.
// This is called by the systemd service via "datadog-installer apm instrument-start host"
// and must not attempt to manage systemd (it would loop).
func (a *InjectorInstaller) InstrumentLDPreload(ctx context.Context) (err error) {
	// Always verify the real launcher payload (under installPath) before
	// touching the symlink or ld.so.preload, so a broken launcher is never
	// activated — neither directly nor through the tmpfs symlink.
	ociLauncherPath := path.Join(a.installPath, "inject", "launcher.preload.so")
	log.Infof("Verifying APM injector launcher %s", ociLauncherPath)
	if err := a.verifySharedLib(ctx, ociLauncherPath); err != nil {
		return err
	}

	// On systemd-managed hosts, reference the launcher through the tmpfs
	// symlink instead of its persistent path (created only now that the
	// launcher has been verified good).
	if a.useTmpfsLink {
		if err := a.enableTmpfsLink(); err != nil {
			return err
		}
	}

	launcherPath := a.ldPreloadEntry()
	log.Infof("Adding APM injector launcher %s to %s", launcherPath, ldSoPreloadPath)
	a.cleanups = append(a.cleanups, a.ldPreloadFileInstrument.cleanup)
	rollback, err := a.ldPreloadFileInstrument.mutate(ctx)
	if err != nil {
		return err
	}
	a.rollbacks = append(a.rollbacks, rollback)
	log.Infof("APM injector launcher present in %s", ldSoPreloadPath)
	return nil
}

// UninstrumentLDPreload directly removes the injector library from /etc/ld.so.preload.
// This is called by the systemd service via "datadog-installer apm instrument-stop host"
// and must not attempt to manage systemd (it would loop).
func (a *InjectorInstaller) UninstrumentLDPreload(ctx context.Context) error {
	log.Infof("Removing APM injector launcher from %s", ldSoPreloadPath)
	_, err := a.ldPreloadFileUninstrument.mutate(ctx)
	if err != nil {
		return err
	}
	// Best-effort cleanup of the tmpfs symlink. It lives on tmpfs and is gone
	// after a reboot regardless, but remove it eagerly so the host is left
	// uninstrumented immediately.
	if a.tmpfsInjectDir != "" {
		if rmErr := os.Remove(a.tmpfsInjectDir); rmErr != nil && !os.IsNotExist(rmErr) {
			log.Warnf("failed to remove tmpfs injector symlink %s: %v", a.tmpfsInjectDir, rmErr)
		}
	}
	log.Infof("APM injector launcher removed from %s", ldSoPreloadPath)
	return nil
}

// addInstrumentScripts writes the instrument scripts that come with the APM injector
// and override the previous instrument scripts if they exist
// These scripts are either:
// - Referenced in our public documentation, so we override them to use installer commands for consistency
// - Used on deb/rpm removal and may break the OCI in the process
func (a *InjectorInstaller) addInstrumentScripts(ctx context.Context) (err error) {
	span, _ := telemetry.StartSpanFromContext(ctx, "add_instrument_scripts")
	defer func() { span.Finish(err) }()

	hostMutator := newFileMutator(
		"/usr/bin/dd-host-install",
		func(_ context.Context, _ []byte) ([]byte, error) {
			return embedded.ScriptDDHostInstall, nil
		},
		nil, nil,
	)
	a.cleanups = append(a.cleanups, hostMutator.cleanup)
	rollbackHost, err := hostMutator.mutate(ctx)
	if err != nil {
		return fmt.Errorf("failed to override dd-host-install: %w", err)
	}
	a.rollbacks = append(a.rollbacks, rollbackHost)
	err = os.Chmod("/usr/bin/dd-host-install", 0755)
	if err != nil {
		return fmt.Errorf("failed to change permissions of dd-host-install: %w", err)
	}

	containerMutator := newFileMutator(
		"/usr/bin/dd-container-install",
		func(_ context.Context, _ []byte) ([]byte, error) {
			return embedded.ScriptDDContainerInstall, nil
		},
		nil, nil,
	)
	a.cleanups = append(a.cleanups, containerMutator.cleanup)
	rollbackContainer, err := containerMutator.mutate(ctx)
	if err != nil {
		return fmt.Errorf("failed to override dd-host-install: %w", err)
	}
	a.rollbacks = append(a.rollbacks, rollbackContainer)
	err = os.Chmod("/usr/bin/dd-container-install", 0755)
	if err != nil {
		return fmt.Errorf("failed to change permissions of dd-container-install: %w", err)
	}

	// Only override dd-cleanup if it exists
	_, err = os.Stat("/usr/bin/dd-cleanup")
	if err == nil {
		cleanupMutator := newFileMutator(
			"/usr/bin/dd-cleanup",
			func(_ context.Context, _ []byte) ([]byte, error) {
				return embedded.ScriptDDCleanup, nil
			},
			nil, nil,
		)
		a.cleanups = append(a.cleanups, cleanupMutator.cleanup)
		rollbackCleanup, err := cleanupMutator.mutate(ctx)
		if err != nil {
			return fmt.Errorf("failed to override dd-cleanup: %w", err)
		}
		a.rollbacks = append(a.rollbacks, rollbackCleanup)
		err = os.Chmod("/usr/bin/dd-cleanup", 0755)
		if err != nil {
			return fmt.Errorf("failed to change permissions of dd-cleanup: %w", err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to check if dd-cleanup exists on disk: %w", err)
	}
	return nil
}

// removeInstrumentScripts removes the install scripts that come with the APM injector
// if and only if they've been installed by the installer and not modified
func (a *InjectorInstaller) removeInstrumentScripts(ctx context.Context) (retErr error) {
	span, _ := telemetry.StartSpanFromContext(ctx, "remove_instrument_scripts")
	defer func() { span.Finish(retErr) }()

	for _, script := range []string{"dd-host-install", "dd-container-install", "dd-cleanup"} {
		path := filepath.Join("/usr/bin", script)
		_, err := os.Stat(path)
		if err == nil {
			err = os.Remove(path)
			if err != nil {
				return fmt.Errorf("failed to remove %s: %w", path, err)
			}
		}
	}
	return nil
}

func (a *InjectorInstaller) addLocalStableConfig(ctx context.Context) (err error) {
	span, _ := telemetry.StartSpanFromContext(ctx, "add_local_stable_config")
	defer func() { span.Finish(err) }()

	appMonitoringConfigMutator := newFileMutator(
		localStableConfigPath,
		func(_ context.Context, existing []byte) ([]byte, error) {
			cfg := config.ApplicationMonitoringConfig{
				Default: config.APMConfigurationDefault{},
			}
			hasChanged := false

			if len(existing) > 0 {
				err := yaml.Unmarshal(existing, &cfg)
				if err != nil {
					return nil, fmt.Errorf("failed to unmarshal existing application_monitoring.yaml: %w", err)
				}
			}

			if a.Env.InstallScript.RuntimeMetricsEnabled != nil {
				hasChanged = true
				cfg.Default.RuntimeMetricsEnabled = a.Env.InstallScript.RuntimeMetricsEnabled
			}
			if a.Env.InstallScript.LogsInjection != nil {
				hasChanged = true
				cfg.Default.LogsInjection = a.Env.InstallScript.LogsInjection
			}
			if a.Env.InstallScript.APMTracingEnabled != nil {
				hasChanged = true
				cfg.Default.APMTracingEnabled = a.Env.InstallScript.APMTracingEnabled
			}
			if a.Env.InstallScript.DataStreamsEnabled != nil {
				hasChanged = true
				cfg.Default.DataStreamsEnabled = a.Env.InstallScript.DataStreamsEnabled
			}
			if a.Env.InstallScript.AppsecEnabled != nil {
				hasChanged = true
				cfg.Default.AppsecEnabled = a.Env.InstallScript.AppsecEnabled
			}
			if a.Env.InstallScript.IastEnabled != nil {
				hasChanged = true
				cfg.Default.IastEnabled = a.Env.InstallScript.IastEnabled
			}
			if a.Env.InstallScript.DataJobsEnabled != nil {
				hasChanged = true
				cfg.Default.DataJobsEnabled = a.Env.InstallScript.DataJobsEnabled
			}
			if a.Env.InstallScript.AppsecScaEnabled != nil {
				hasChanged = true
				cfg.Default.AppsecScaEnabled = a.Env.InstallScript.AppsecScaEnabled
			}
			if a.Env.InstallScript.ProfilingEnabled != "" {
				hasChanged = true
				cfg.Default.ProfilingEnabled = &a.Env.InstallScript.ProfilingEnabled
			}
			if a.Env.InstallScript.TracerLogsCollectionEnabled != nil {
				hasChanged = true
				cfg.Default.LogsCollectionEnabled = a.Env.InstallScript.TracerLogsCollectionEnabled
			}
			if a.Env.InstallScript.RumEnabled != nil {
				hasChanged = true
				cfg.Default.RumEnabled = a.Env.InstallScript.RumEnabled
			}
			if a.Env.InstallScript.RumApplicationID != "" {
				hasChanged = true
				cfg.Default.RumApplicationID = a.Env.InstallScript.RumApplicationID
			}
			if a.Env.InstallScript.RumClientToken != "" {
				hasChanged = true
				cfg.Default.RumClientToken = a.Env.InstallScript.RumClientToken
			}
			if a.Env.InstallScript.RumRemoteConfigurationID != "" {
				hasChanged = true
				cfg.Default.RumRemoteConfigurationID = a.Env.InstallScript.RumRemoteConfigurationID
			}
			if a.Env.InstallScript.RumSite != "" {
				hasChanged = true
				cfg.Default.RumSite = a.Env.InstallScript.RumSite
			}

			// Avoid creating a .backup file and overwriting the existing file if no changes were made
			if hasChanged {
				return yaml.Marshal(cfg)
			}
			return existing, nil
		},
		nil, nil,
	)
	rollback, err := appMonitoringConfigMutator.mutate(ctx)
	if err != nil {
		return err
	}
	err = os.Chmod(localStableConfigPath, 0644)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to set permissions for application_monitoring.yaml: %w", err)
	}

	a.rollbacks = append(a.rollbacks, rollback)
	return nil
}

func shouldInstrumentHost(execEnvs *env.Env) bool {
	switch execEnvs.InstallScript.APMInstrumentationEnabled {
	case env.APMInstrumentationEnabledHost, env.APMInstrumentationEnabledAll, env.APMInstrumentationNotSet:
		return true
	case env.APMInstrumentationEnabledDocker:
		return false
	default:
		log.Warnf("Unknown value for DD_APM_INSTRUMENTATION_ENABLED: %s. Supported values are all/docker/host", execEnvs.InstallScript.APMInstrumentationEnabled)
		return false
	}
}

func shouldInstrumentDocker(execEnvs *env.Env) bool {
	switch execEnvs.InstallScript.APMInstrumentationEnabled {
	case env.APMInstrumentationEnabledDocker, env.APMInstrumentationEnabledAll, env.APMInstrumentationNotSet:
		return true
	case env.APMInstrumentationEnabledHost:
		return false
	default:
		log.Warnf("Unknown value for DD_APM_INSTRUMENTATION_ENABLED: %s. Supported values are all/docker/host", execEnvs.InstallScript.APMInstrumentationEnabled)
		return false
	}
}

func mustInstrumentDocker(execEnvs *env.Env) bool {
	return execEnvs.InstallScript.APMInstrumentationEnabled == env.APMInstrumentationEnabledDocker
}
