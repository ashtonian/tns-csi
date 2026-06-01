// Package main implements the TrueNAS CSI driver entry point.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/fenio/tns-csi/pkg/driver"
	"github.com/fenio/tns-csi/pkg/metrics"
	"k8s.io/klog/v2"
)

// Build-time variables set via -ldflags.
var (
	version   = "dev"
	gitCommit = "unknown"
	buildDate = "unknown"
)

var (
	endpoint                  = flag.String("endpoint", "unix:///var/lib/kubelet/plugins/tns.csi.io/csi.sock", "CSI endpoint")
	nodeID                    = flag.String("node-id", "", "Node ID")
	driverName                = flag.String("driver-name", "tns.csi.io", "Name of the driver")
	apiURL                    = flag.String("api-url", "", "Storage system API URL (e.g., ws://10.10.20.100/api/v2.0/websocket)")
	apiKey                    = flag.String("api-key", "", "Storage system API key")
	metricsAddr               = flag.String("metrics-addr", ":8080", "Address to expose Prometheus metrics")
	skipTLSVerify             = flag.Bool("skip-tls-verify", false, "Skip TLS certificate verification (for self-signed certificates)")
	showVersion               = flag.Bool("show-version", false, "Show version and exit")
	debug                     = flag.Bool("debug", false, "Enable debug logging (equivalent to -v=4)")
	enableNVMeDiscovery       = flag.Bool("enable-nvme-discovery", false, "Run nvme discover before nvme connect (default: false, all connection params are known from volume context)")
	maxConcurrentNVMeConnects = flag.Int("max-concurrent-nvme-connects", 5, "Maximum number of concurrent NVMe-oF connect operations per node (limits kernel NVMe subsystem lock contention)")
	dashboardAddr             = flag.String("dashboard-addr", "", "Address for in-cluster web dashboard (e.g., ':2137', empty = disabled)")
	dashboardPool             = flag.String("dashboard-pool", "", "ZFS pool for unmanaged volume discovery in dashboard")
	clusterID                 = flag.String("cluster-id", "", "Unique identifier for this cluster (for multi-cluster TrueNAS sharing)")

	// Filesystem auto-recovery for block volumes (NVMe-oF, iSCSI). All default
	// off/safe; see docs/RFC-AUTO-RECOVERY.md.
	autoRecovery            = flag.String("auto-recovery", "off", "Filesystem auto-recovery mode: off|shadow|on (shadow detects and logs without acting)")
	autoRecoveryRepair      = flag.Bool("auto-recovery-repair", false, "Allow non-destructive repair at stage time (xfs_repair clean-log / e2fsck -p)")
	autoRecoveryDestructive = flag.Bool("auto-recovery-repair-destructive", false, "Allow data-losing repair (xfs_repair -L / e2fsck -fy); requires -auto-recovery-repair")
	autoRecoverySnapshot    = flag.Bool("auto-recovery-snapshot", true, "Take a ZFS snapshot before any mutating repair")
	autoRecoveryEvict       = flag.String("auto-recovery-evict", "evict", "How the reconciler removes a pod: evict (PDB-respecting) | delete")
	autoRecoveryDebounce    = flag.Int("auto-recovery-debounce", 3, "Consecutive confirmations required before the reconciler evicts/repairs")
	autoRecoveryCooldown    = flag.Duration("auto-recovery-cooldown", 300*time.Second, "Minimum time between recovery actions for one device")
	autoRecoveryMaxEvict    = flag.Int("auto-recovery-max-evictions", 5, "Max evictions per node per retry window")
	autoRecoveryRepairTO    = flag.Duration("auto-recovery-repair-timeout", 10*time.Minute, "Hard bound on a single repair invocation")
	autoRecoveryRetryWindow = flag.Duration("auto-recovery-retry-window", time.Hour, "Per-device circuit-breaker window")
	autoRecoveryRetries     = flag.Int("auto-recovery-retries", 3, "Per-device recovery attempts allowed within the retry window")
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	// Enable debug logging if --debug flag or DEBUG_CSI env var is set
	if *debug || os.Getenv("DEBUG_CSI") == "true" || os.Getenv("DEBUG_CSI") == "1" {
		if err := flag.Set("v", "4"); err != nil {
			klog.Warningf("Failed to set verbosity level: %v", err)
		}
	}

	if *showVersion {
		fmt.Printf("%s version: %s\n", *driverName, version)
		fmt.Printf("  Git commit: %s\n", gitCommit)
		fmt.Printf("  Build date: %s\n", buildDate)
		fmt.Printf("  Go version: %s\n", runtime.Version())
		fmt.Printf("  Platform:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	if *nodeID == "" {
		klog.Fatal("Node ID must be provided")
	}

	if *apiURL == "" {
		klog.Fatal("Storage API URL must be provided")
	}

	if *apiKey == "" {
		klog.Fatal("Storage API key must be provided")
	}

	recoveryMode, err := driver.ParseRecoveryMode(*autoRecovery)
	if err != nil {
		klog.Fatalf("Invalid -auto-recovery flag: %v", err)
	}
	if *autoRecoveryEvict != driver.EvictModeEviction && *autoRecoveryEvict != driver.EvictModeDelete {
		klog.Fatalf("Invalid -auto-recovery-evict flag %q (want evict|delete)", *autoRecoveryEvict)
	}
	recoveryCfg := driver.RecoveryConfig{
		Mode:              recoveryMode,
		Repair:            *autoRecoveryRepair,
		RepairDestructive: *autoRecoveryDestructive,
		Snapshot:          *autoRecoverySnapshot,
		EvictMode:         *autoRecoveryEvict,
		Debounce:          *autoRecoveryDebounce,
		Cooldown:          *autoRecoveryCooldown,
		MaxEvictions:      *autoRecoveryMaxEvict,
		RepairTimeout:     *autoRecoveryRepairTO,
		RetryWindow:       *autoRecoveryRetryWindow,
		MaxRetries:        *autoRecoveryRetries,
	}
	if recoveryCfg.Enabled() {
		klog.Infof("Filesystem auto-recovery enabled: mode=%s repair=%v destructive=%v snapshot=%v evict=%s",
			recoveryCfg.Mode, recoveryCfg.Repair, recoveryCfg.RepairDestructive, recoveryCfg.Snapshot, recoveryCfg.EvictMode)
	}

	// Set version info for metrics endpoint
	metrics.SetVersionInfo(version, gitCommit, buildDate)

	klog.Infof("Starting TNS CSI Driver %s (commit: %s, built: %s)", version, gitCommit, buildDate)
	klog.V(4).Infof("Driver: %s", *driverName)
	klog.V(4).Infof("Node ID: %s", *nodeID)

	drv, err := driver.NewDriver(driver.Config{
		DriverName:                *driverName,
		Version:                   version,
		NodeID:                    *nodeID,
		Endpoint:                  *endpoint,
		APIURL:                    *apiURL,
		APIKey:                    *apiKey,
		MetricsAddr:               *metricsAddr,
		SkipTLSVerify:             *skipTLSVerify,
		EnableNVMeDiscovery:       *enableNVMeDiscovery,
		MaxConcurrentNVMeConnects: *maxConcurrentNVMeConnects,
		DashboardAddr:             *dashboardAddr,
		DashboardPool:             *dashboardPool,
		ClusterID:                 *clusterID,
		Recovery:                  recoveryCfg,
	})
	if err != nil {
		klog.Fatalf("Failed to create driver: %v", err)
	}

	if err := drv.Run(); err != nil {
		klog.Fatalf("Failed to run driver: %v", err)
	}
}
