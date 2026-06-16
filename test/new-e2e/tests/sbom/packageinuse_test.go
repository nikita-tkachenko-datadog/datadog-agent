// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025-present Datadog, Inc.

package sbom

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DataDog/agent-payload/v5/cyclonedx_v1_4"
	"github.com/DataDog/agent-payload/v5/sbom"

	"github.com/DataDog/datadog-agent/test/e2e-framework/components/datadog/apps/sbomtargets"
	"github.com/DataDog/datadog-agent/test/e2e-framework/components/datadog/kubernetesagentparams"
	e2eos "github.com/DataDog/datadog-agent/test/e2e-framework/components/os"
	scenec2 "github.com/DataDog/datadog-agent/test/e2e-framework/scenarios/aws/ec2"
	"github.com/DataDog/datadog-agent/test/e2e-framework/scenarios/aws/fakeintake"
	scenkubeadm "github.com/DataDog/datadog-agent/test/e2e-framework/scenarios/aws/kubeadm"
	"github.com/DataDog/datadog-agent/test/e2e-framework/testing/e2e"
	"github.com/DataDog/datadog-agent/test/e2e-framework/testing/environments"
	provkubeadm "github.com/DataDog/datadog-agent/test/e2e-framework/testing/provisioners/aws/kubernetes/kubeadm"
	"github.com/DataDog/datadog-agent/test/fakeintake/aggregator"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
)

// Runtime-usage ("package in use") properties merged onto the container-image
// SBOM components by the core-agent SBOM collector from the system-probe SBOM
// resolver. "Not in use" is reported as LastSeenRunning == "0"; "in use" is a
// recent Unix timestamp (seconds). HasSetSuidBit / RunningAsRoot are "true" /
// "false". See pkg/security/resolvers/sbom/report.go and
// comp/core/workloadmeta/collectors/internal/remote/sbomcollector.
const (
	propLastSeenRunning = "LastSeenRunning"
	propHasSetSuidBit   = "HasSetSuidBit"
	propRunningAsRoot   = "RunningAsRoot"
)

const (
	// inUsePackage is an rpm package present in the ubi9 image whose binary is
	// not executed by the idle `tail -f /dev/null` workload, so it starts "not
	// in use" and flips to "in use" only once the service below runs it. It is an
	// OS (rpm) package so the system-probe scanner and Trivy agree on
	// name@version and the collector's merge attaches the runtime properties.
	inUsePackage = "gzip"
	inUseBinary  = "gzip"

	// inUseWindow is the maximum age a LastSeenRunning timestamp may have while we
	// consider the package "in use". It must comfortably exceed the end-to-end
	// latency (enrichment forward + container SBOM periodic refresh) so a freshly
	// re-emitted payload still reads as recent.
	inUseWindowSec = int64(150)
	// staleWindow is the age past which a frozen LastSeenRunning is considered
	// "no longer in use". Greater than inUseWindow so the two phases never overlap.
	staleWindowSec = int64(210)
)

// packageInUseHelmValues extends the container-image SBOM Helm values
// (overlayfs direct scan, os+languages analyzers) with everything needed for the
// "package in use" enrichment on a containerd kubeadm node:
//   - the system-probe security module + SBOM resolver
//     (DD_RUNTIME_SECURITY_CONFIG_SBOM_ENABLED) that tracks which packages a
//     running process accesses, and
//   - the core-agent enrichment collector (DD_SBOM_ENRICHMENT_USAGE_ENABLED) that
//     merges those runtime properties onto the Trivy container-image SBOM.
//
// The enrichment/forward intervals are shortened so a package's in-use timestamp
// surfaces within the test window instead of the 1m default.
func packageInUseHelmValues() string {
	return `datadog:
  criSocketPath: /run/containerd/containerd.sock
  kubelet:
    tlsVerify: false
  useHostPID: true
  securityAgent:
    runtime:
      enabled: true
  sbom:
    containerImage:
      enabled: true
      uncompressedLayersSupport: true
      overlayFSDirectScan: true
      analyzers: ["os", "languages"]
agents:
  useHostNetwork: true
  containers:
    agent:
      env:
        - name: DD_SBOM_ENRICHMENT_USAGE_ENABLED
          value: "true"
    systemProbe:
      env:
        # The UsageConsumer that registers the SBOMCollector gRPC stream the core
        # agent consumes is created by system-probe only when it sees
        # sbom.enrichment.usage.enabled, so this must be set on the system-probe
        # container too (not just the core agent), else the agent's collector gets
        # "unknown service datadog.sbom.SBOMCollector".
        - name: DD_SBOM_ENRICHMENT_USAGE_ENABLED
          value: "true"
        - name: DD_RUNTIME_SECURITY_CONFIG_SBOM_ENABLED
          value: "true"
        - name: DD_RUNTIME_SECURITY_CONFIG_SBOM_ENRICHMENT_INTERVAL
          value: "10s"
        - name: DD_RUNTIME_SECURITY_CONFIG_SBOM_ENRICHMENT_TICKER
          value: "10s"
        # forward_interval x maxRetryForwarding(10) is the window the resolver
        # waits for the image's Trivy SBOM to be available before giving up
        # forwarding for good. Keep it wide enough to outlast the initial
        # overlayfs Trivy scans (a 5s interval gave up after ~50s, before the
        # container SBOMs were ready).
        - name: DD_RUNTIME_SECURITY_CONFIG_SBOM_FORWARD_INTERVAL
          value: "30s"
  volumeMounts:
    - name: trivycache
      mountPath: /root/.cache/trivy
    - name: imageoverlay
      mountPath: /var/lib/containerd
      readOnly: true
  volumes:
    - name: trivycache
      emptyDir: {}
    - name: imageoverlay
      hostPath:
        path: /var/lib/containerd
`
}

type packageInUseSuite struct {
	baseSuite[environments.Kubernetes]
}

// TestSBOMPackageInUseKubeadmSuite provisions the same RHEL 10 single-node
// kubeadm cluster as TestSBOMKubeadmSuite, but additionally enables the CWS SBOM
// resolver and the core-agent usage enrichment, then verifies the "package in
// use" feature end to end against the ubi9 workload: a package goes from not in
// use, to in use once a service runs its binary, and back to stale once the
// service stops.
func TestSBOMPackageInUseKubeadmSuite(t *testing.T) {
	prov := provkubeadm.Provisioner(
		provkubeadm.WithRunOptions(
			scenkubeadm.WithVMOptions(
				scenec2.WithOS(e2eos.RedHat10),
				scenec2.WithInstanceType("t3.2xlarge"),
			),
			scenkubeadm.WithFakeintakeOptions(fakeintake.WithMemory(2048), fakeintake.WithRetentionPeriod(sbomHostRetentionPeriod)),
			scenkubeadm.WithDeploySBOMWorkloads(),
			scenkubeadm.WithAgentOptions(
				kubernetesagentparams.WithDualShipping(),
				kubernetesagentparams.WithTimeout(900),
				kubernetesagentparams.WithHelmValues(packageInUseHelmValues()),
			),
		),
	)
	e2e.Run(t, &packageInUseSuite{}, e2e.WithProvisioner(prov))
}

func (s *packageInUseSuite) SetupSuite() {
	s.baseSuite.SetupSuite()
	s.clusterName = s.Env().KubernetesCluster.ClusterName
	s.Fakeintake = s.Env().FakeIntake.Client()
}

// Test00UpAndRunning waits (the 00 prefix runs it first) for the Agent DaemonSet
// pods - including the security-agent and system-probe containers enabled here -
// to be ready before the package-in-use assertions run.
func (s *packageInUseSuite) Test00UpAndRunning() {
	ctx := context.Background()
	s.EventuallyWithTf(func(c *assert.CollectT) {
		nodes, err := s.Env().KubernetesCluster.Client().CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: fields.OneTermEqualSelector("kubernetes.io/os", "linux").String(),
		})
		require.NoErrorf(c, err, "Failed to list Linux nodes")

		pods, err := s.Env().KubernetesCluster.Client().CoreV1().Pods("datadog").List(ctx, metav1.ListOptions{
			LabelSelector: fields.OneTermEqualSelector("app", s.Env().Agent.LinuxNodeAgent.LabelSelectors["app"]).String(),
		})
		require.NoErrorf(c, err, "Failed to list Linux datadog agent pods")

		assert.Len(c, pods.Items, len(nodes.Items))
		for _, pod := range pods.Items {
			for _, cs := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
				assert.Truef(c, cs.Ready, "Container %s of pod %s isn't ready", cs.Name, pod.Name)
				assert.Zerof(c, cs.RestartCount, "Container %s of pod %s has restarted", cs.Name, pod.Name)
			}
		}
	}, 10*time.Minute, 10*time.Second, "Not all agents eventually became ready in time.")
}

// TestPackageInUse drives the full not-in-use -> in-use -> stale cycle for the
// gzip package of the ubi9 workload.
func (s *packageInUseSuite) TestPackageInUse() {
	ubiRepo := ubiTargetRepo()

	// Keep the idle ubi9 workload forwarding its runtime SBOM steadily (without
	// touching gzip) so the enrichment merge reliably runs once its image SBOM lands.
	s.keepUbiActive()

	// Phase 1: baseline. The enrichment merge must have run (gzip carries a
	// LastSeenRunning property at all) and gzip must be reported "not in use"
	// (LastSeenRunning == "0"): the idle workload only runs `tail`, never gzip.
	s.Run("not-in-use", func() {
		s.EventuallyWithTf(func(collect *assert.CollectT) {
			c := &myCollectT{CollectT: collect, errors: []error{}}
			collect = nil //nolint:ineffassign

			ts, present, inUse := s.gzipUsage(c, ubiRepo)
			require.Truef(c, present, "no enriched ubi9 SBOM yet (gzip carries no %s property)", propLastSeenRunning)
			s.T().Logf("PKG-IN-USE baseline: gzip LastSeenRunning=%d; in-use components=%v", ts, inUse)
			assert.Zerof(c, ts, "gzip should be not-in-use at baseline, got LastSeenRunning=%d", ts)
			// 14m: the enrichment can only merge once the ubi9 overlayfs Trivy SBOM is
			// ready in workloadmeta, which lands ~10-15m into the run.
		}, 14*time.Minute, 15*time.Second, "ubi9 SBOM never reported gzip as not-in-use")
	})

	// Phase 2: start a service that repeatedly runs gzip, and verify the package
	// flips to in-use (a recent LastSeenRunning timestamp).
	s.Run("in-use", func() {
		s.startInUseService()

		s.EventuallyWithTf(func(collect *assert.CollectT) {
			c := &myCollectT{CollectT: collect, errors: []error{}}
			collect = nil //nolint:ineffassign

			ts, present, inUse := s.gzipUsage(c, ubiRepo)
			require.Truef(c, present, "gzip carries no %s property", propLastSeenRunning)
			require.Positivef(c, ts, "gzip still reported not-in-use (LastSeenRunning=0); in-use components=%v", inUse)
			age := time.Now().Unix() - ts
			s.T().Logf("PKG-IN-USE running: gzip LastSeenRunning=%d age=%ds; in-use components=%v", ts, age, inUse)
			assert.LessOrEqualf(c, age, inUseWindowSec, "gzip LastSeenRunning is %ds old, expected <= %ds while in use", age, inUseWindowSec)
			// The workload runs as root and gzip is not a setuid binary, so the
			// security enrichment must reflect that on the in-use component.
			assert.Equalf(c, "true", s.gzipProperty(ubiRepo, propRunningAsRoot), "gzip RunningAsRoot should be true (ubi9 workload runs as root)")
			assert.Equalf(c, "false", s.gzipProperty(ubiRepo, propHasSetSuidBit), "gzip HasSetSuidBit should be false (gzip is not setuid)")
		}, 5*time.Minute, 15*time.Second, "ubi9 SBOM never reported gzip as in-use after starting the service")
	})

	// Phase 3: stop the service and verify gzip ages out of the in-use window.
	// LastSeenRunning is a monotonic "last seen" timestamp that is not reset on
	// process exit, so "back to not in use" is observed as the timestamp freezing
	// and growing stale rather than returning to "0".
	s.Run("stale-after-stop", func() {
		s.stopInUseService()

		s.EventuallyWithTf(func(collect *assert.CollectT) {
			c := &myCollectT{CollectT: collect, errors: []error{}}
			collect = nil //nolint:ineffassign

			ts, _, _ := s.gzipUsage(c, ubiRepo)
			require.Positivef(c, ts, "gzip was never reported in-use, cannot assert staleness")
			age := time.Now().Unix() - ts
			s.T().Logf("PKG-IN-USE stopped: gzip LastSeenRunning=%d age=%ds", ts, age)
			assert.Greaterf(c, age, staleWindowSec, "gzip LastSeenRunning is only %ds old, expected > %ds (stale/not running)", age, staleWindowSec)
		}, 5*time.Minute, 20*time.Second, "ubi9 SBOM never reported gzip as stale after stopping the service")
	})
}

// gzipUsage returns, across every successful ubi9 container-image SBOM payload
// retained by fakeintake, the highest LastSeenRunning timestamp reported for the
// gzip component (newest access wins), whether that property was present on any
// payload, and the names of all components currently reported as in use (a
// diagnostic for when the targeted package is not the one that flipped).
func (s *packageInUseSuite) gzipUsage(c *myCollectT, ubiRepo string) (maxTS int64, present bool, inUse []string) {
	ids, err := s.Fakeintake.GetSBOMIDs()
	require.NoErrorf(c, err, "Failed to query fake intake")
	ids = lo.Filter(ids, func(id string, _ int) bool { return strings.Contains(id, ubiRepo+"@") })
	if len(ids) == 0 {
		s.dumpSBOMInventory()
	}
	require.NotEmptyf(c, ids, "No ubi9 SBOM id yet")

	payloads := lo.FlatMap(ids, func(id string, _ int) []*aggregator.SBOMPayload {
		p, err := s.Fakeintake.FilterSBOMs(id)
		assert.NoErrorf(c, err, "Failed to query fake intake")
		return p
	})
	payloads = lo.Filter(payloads, func(p *aggregator.SBOMPayload, _ int) bool {
		return p.GetType() == sbom.SBOMSourceType_CONTAINER_IMAGE_LAYERS &&
			p.Status == sbom.SBOMStatus_SUCCESS && p.GetCyclonedx() != nil
	})
	require.NotEmptyf(c, payloads, "No successful ubi9 container SBOM yet")

	seen := map[string]struct{}{}
	for _, p := range payloads {
		for _, comp := range p.GetCyclonedx().Components {
			ts, ok := lastSeenRunning(comp)
			if !ok {
				continue
			}
			if comp.GetName() == inUsePackage {
				present = true
				if ts > maxTS {
					maxTS = ts
				}
			}
			if ts > 0 {
				if _, dup := seen[comp.GetName()]; !dup {
					seen[comp.GetName()] = struct{}{}
					inUse = append(inUse, comp.GetName())
				}
			}
		}
	}
	return maxTS, present, inUse
}

// gzipProperty returns the value of the named runtime property on the gzip
// component from the most recently collected ubi9 SBOM payload, or "" if absent.
func (s *packageInUseSuite) gzipProperty(ubiRepo, name string) string {
	ids, err := s.Fakeintake.GetSBOMIDs()
	if err != nil {
		return ""
	}
	ids = lo.Filter(ids, func(id string, _ int) bool { return strings.Contains(id, ubiRepo+"@") })
	var value string
	var newest time.Time
	for _, id := range ids {
		payloads, err := s.Fakeintake.FilterSBOMs(id)
		if err != nil {
			continue
		}
		for _, p := range payloads {
			if p.GetType() != sbom.SBOMSourceType_CONTAINER_IMAGE_LAYERS || p.Status != sbom.SBOMStatus_SUCCESS || p.GetCyclonedx() == nil {
				continue
			}
			if comp := findComponent(p.GetCyclonedx().Components, inUsePackage); comp != nil {
				if vals := propertyValues(comp.GetProperties(), name); len(vals) > 0 && !p.GetCollectedTime().Before(newest) {
					newest = p.GetCollectedTime()
					value = vals[len(vals)-1]
				}
			}
		}
	}
	return value
}

// startInUseService launches, inside the ubi9 workload pod, a detached loop that
// repeatedly executes the gzip binary so the package is continuously seen
// running. The loop's pid is recorded so stopInUseService can stop it.
func (s *packageInUseSuite) startInUseService() {
	// Run the binary every 15s (> the 10s enrichment interval) so each execution
	// re-arms the resolver's forwarding debouncer: a tighter loop only forwards
	// once (the resolver suppresses re-forwards within the enrichment interval),
	// which is fragile if that single forward races the image SBOM becoming ready.
	script := fmt.Sprintf(`nohup sh -c 'echo $$ > /tmp/inuse.pid; while true; do %s --version >/dev/null 2>&1; sleep 15; done' </dev/null >/dev/null 2>&1 &`, inUseBinary)
	stdout, stderr := s.podExec("sh", "-c", script)
	s.T().Logf("PKG-IN-USE start service: stdout=%q stderr=%q", stdout, stderr)
}

// stopInUseService stops the in-use loop started by startInUseService.
func (s *packageInUseSuite) stopInUseService() {
	stdout, stderr := s.podExec("sh", "-c", `kill "$(cat /tmp/inuse.pid)" 2>/dev/null; rm -f /tmp/inuse.pid; echo stopped`)
	s.T().Logf("PKG-IN-USE stop service: stdout=%q stderr=%q", stdout, stderr)
}

// podExec runs cmd in the ubi9 workload pod's container and returns stdout/stderr.
func (s *packageInUseSuite) podExec(cmd ...string) (string, string) {
	pods, err := s.Env().KubernetesCluster.Client().CoreV1().Pods(sbomtargets.Namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", ubiWorkloadName).String(),
	})
	require.NoErrorf(s.T(), err, "failed to list ubi9 workload pods")
	require.NotEmptyf(s.T(), pods.Items, "no ubi9 workload pod found in namespace %s", sbomtargets.Namespace)

	stdout, stderr, err := s.Env().KubernetesCluster.KubernetesClient.PodExec(sbomtargets.Namespace, pods.Items[0].Name, "main", cmd)
	require.NoErrorf(s.T(), err, "pod exec failed: %s", stderr)
	return stdout, stderr
}

// TestZZDumpAgentDiagnostics runs last (the ZZ prefix sorts it after the
// package-in-use test) and dumps the Agent's SBOM/CWS state - the SBOM status
// section and the system-probe/security logs - which otherwise live only in the
// flare artifact, not the test trace. It is a debugging aid and always passes.
func (s *packageInUseSuite) TestZZDumpAgentDiagnostics() {
	pods, err := s.Env().KubernetesCluster.Client().CoreV1().Pods("datadog").List(context.Background(), metav1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", s.Env().Agent.LinuxNodeAgent.LabelSelectors["app"]).String(),
	})
	if err != nil || len(pods.Items) == 0 {
		s.T().Logf("DIAG: could not list agent pods: %v", err)
		return
	}
	pod := pods.Items[0].Name

	// Best-effort probes confirming the enrichment is wired up: the usage flag on
	// the agent, the resolved runtime_security_config, and the shared
	// runtime-security command socket the core agent's collector connects to.
	for _, step := range []struct {
		label string
		cmd   []string
	}{
		{"agent-env", []string{"sh", "-c", "env | grep -iE 'DD_SBOM|DD_RUNTIME_SECURITY' | sort"}},
		{"agent-config", []string{"sh", "-c", "agent config 2>/dev/null | grep -iE 'enrichment|runtime_security_config' | head -40"}},
		{"agent-sockets", []string{"sh", "-c", "ls -la /var/run/sysprobe/ 2>&1"}},
	} {
		stdout, stderr, err := s.Env().KubernetesCluster.KubernetesClient.PodExec("datadog", pod, "agent", step.cmd)
		s.T().Logf("DIAG[%s] err=%v\n%s\n%s", step.label, err, stdout, stderr)
	}
}

// keepUbiActive starts, inside the ubi9 workload pod, a detached loop that
// continuously accesses a non-gzip file. An idle container (its entrypoint is
// `tail -f /dev/null`) forwards its runtime SBOM only a handful of times and can
// miss the window once its image SBOM is available; keeping it active makes the
// resolver re-forward steadily, the way the always-busy Agent containers do.
// gzip is never touched here, so it stays not-in-use until the in-use phase.
func (s *packageInUseSuite) keepUbiActive() {
	script := `nohup sh -c 'while true; do cat /etc/os-release >/dev/null 2>&1; sleep 12; done' </dev/null >/dev/null 2>&1 &`
	stdout, stderr := s.podExec("sh", "-c", script)
	s.T().Logf("PKG-IN-USE keepalive: stdout=%q stderr=%q", stdout, stderr)
}

// pkgInUseInventoryOnce guards dumpSBOMInventory so the inventory is logged at
// most once even though it is called from a retry loop.
var pkgInUseInventoryOnce sync.Once

// dumpSBOMInventory logs every SBOM id with its type and status once, to diagnose
// a missing ubi9 payload (e.g. the image was never scanned or never enriched).
func (s *packageInUseSuite) dumpSBOMInventory() {
	pkgInUseInventoryOnce.Do(func() {
		ids, err := s.Fakeintake.GetSBOMIDs()
		if err != nil {
			s.T().Logf("PKG-IN-USE inventory: GetSBOMIDs error: %v", err)
			return
		}
		for _, id := range ids {
			ps, err := s.Fakeintake.FilterSBOMs(id)
			if err != nil {
				continue
			}
			for _, p := range ps {
				s.T().Logf("PKG-IN-USE inventory id=%q type=%v status=%v", id, p.GetType(), p.Status)
			}
		}
	})
}

// lastSeenRunning parses the LastSeenRunning property of a component into a Unix
// timestamp. The second return is false when the component carries no such
// property (i.e. the runtime enrichment has not been merged onto it yet).
func lastSeenRunning(comp *cyclonedx_v1_4.Component) (int64, bool) {
	vals := propertyValues(comp.GetProperties(), propLastSeenRunning)
	if len(vals) == 0 {
		return 0, false
	}
	var maxTS int64
	for _, v := range vals {
		if ts, err := strconv.ParseInt(v, 10, 64); err == nil && ts > maxTS {
			maxTS = ts
		}
	}
	return maxTS, true
}

// ubiWorkloadName is the sbomtargets deployment/label of the ubi9 workload.
const ubiWorkloadName = "sbom-ubi9"

// ubiTargetRepo returns the ubi9 image repo from the shared containerTargets
// table so the SBOM id filter stays in sync with the workload it scans.
func ubiTargetRepo() string {
	for _, t := range containerTargets {
		if t.short == "ubi" {
			return t.repo
		}
	}
	return "registry.access.redhat.com/ubi9/ubi"
}
