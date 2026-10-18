# Bounded node volume observations

`NodeGetVolumeHealth` reports what the node can observe about a volume. Health
checks do not disconnect, unmount, format, repair, restart pods or take ownership.
Normal staging also checks an existing NVMe filesystem mount before reporting
success, retaining its consumers when that observation fails.

## Results

| Evidence | Result |
| --- | --- |
| Mount and NVMe identity remain consistent; required read-only operations complete | No adverse CSI health entry |
| Read-only filesystem operation returns an I/O error | `INACCESSIBLE` |
| XFS root-inode query succeeds but reports sick metadata | `DEGRADED`; already-mounted staging refuses success |
| Unsupported capability, unreadable controller, missing identity, occupied probe slot, timeout or changed mount/namespace | Non-OK RPC status; no healthy result |
| Confirmed missing/unmounted path | Inaccessible health; statistics return `NotFound` |

CSI 1.13 makes health paths optional. Without a publish or staging path, the
driver preserves backend lookup errors but returns `Unavailable` for an existing
volume: dataset existence does not establish filesystem liveness on this node.
Clients must handle that status instead of assuming healthy. Actual kubelet and
sidecar support must be verified for the deployment; advertising the alpha CSI
health RPC does not establish that any consumer calls it. The removed legacy
`VolumeCondition` API is not restored by this change. See the
[CSI 1.13 health contract](https://github.com/container-storage-interface/spec/blob/v1.13.0/spec.md#nodegetvolumehealth).

## Consumer compatibility

These are source-checked versions, not evidence about an installed cluster:

| Consumer | Integration with this driver |
| --- | --- |
| [Kubelet 1.37.1](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/volume/csi/csi_client.go) | Calls `NodeGetVolumeHealth` and maps adverse entries to Pod volume-health conditions. |
| [Kubelet 1.36.2](https://github.com/kubernetes/kubernetes/blob/v1.36.2/pkg/volume/csi/csi_client.go) | Reads legacy `VolumeCondition` through `NodeGetVolumeStats`; it does not consume this driver's new health RPC. |
| [Liveness probe 2.14.0](https://github.com/kubernetes-csi/livenessprobe/blob/v2.14.0/cmd/livenessprobe/main.go) | Calls the driver's identity `Probe` RPC. A successful `/healthz` does not report volume liveness. |

In Kubernetes 1.37.1, `CSIVolumeHealth` is
[alpha and disabled by default](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/features/kube_features.go).
The [kubelet health manager](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/kubelet/volumemanager/volume_manager.go)
requires that gate. The API server also needs the gate to retain newly reported
[Pod volume-health status](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/api/pod/util.go).
Installing this chart does not configure either component's feature gates.

The [1.37.1 health manager](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/kubelet/volumemanager/volumehealth/manager.go)
leaves previous conditions unchanged when a health RPC fails. Monitor failed
probes separately: an empty or unchanged cached condition list does not establish
a fresh successful observation. Neither this status update nor `/healthz` requests
application restarts.

Before adoption, verify each node's kubelet version, the API-server version and
effective feature gates, then observe this driver's health RPC and resulting Pod
status in the disposable qualification environment. A chart render, advertised
capability or client-only check does not establish that deployed reporting works.

## Mount and statistics observations

NVMe observations check the actual device number, namespace identity and a live
controller through sysfs ownership, including native multipath. A retained node
record or staging metadata binds that observation to the volume. Legacy volumes
without either cannot receive a healthy NVMe result; supply their staging path
or complete normal staging to establish the binding. Corrupt or conflicting
metadata refuses the observation. Absolute and relative symlinks, including
parent components, are followed inside a bounded helper. The opened descriptor's
device and mount ID must match the captured identity, and the parent checks the
resulting mount-table identity again.

Raw block volumes use a bounded metadata-only preflight to identify the device
before capturing namespace/controller state. The capacity result must refer to
that same device, and namespace identity is checked again afterward. Changed
device numbers, device types or namespaces return unavailable. A raw device whose
identity was not established before observation also refuses the request.

`NodeGetVolumeStats` shares the probe limits and returns observed fd-based
capacity/inode counters. It returns errors on failed or inconsistent observations.
For raw block devices, capacity comes from the device's sysfs sector count;
filesystem usage and inode counts are omitted. Attach, publish and access-mode
semantics are unchanged.

## XFS capability and limits

The helper opens the requested path read-only, verifies its mount ID/device and
reads one directory entry. For XFS it additionally runs this fixed command on an
inherited, pinned descriptor:

```text
xfs_io -r -c "bulkstat_single -v 5 root" /proc/self/fd/5
```

The supported runtime must supply `xfs_io`, v5 root bulkstat support and the
required privilege. The normal image includes xfsprogs and xfsprogs-extra;
custom images and host kernels require capability qualification. Failure to
obtain positive evidence returns unavailable. Successful exit alone is
insufficient: the helper requires the expected v5 output and empty diagnostics.
The inspected [xfsprogs v6.12 source](https://kernel.googlesource.com/pub/scm/fs/xfs/xfsprogs-dev/+/refs/tags/v6.12.0/io/bulkstat.c)
can print a query error while leaving the process exit status successful.

The [Linux v6.12 bulkstat implementation](https://github.com/torvalds/linux/blob/v6.12/fs/xfs/xfs_ioctl.c)
checks privilege and forced-shutdown state before servicing the request. A
geometry query alone does not provide the same shutdown evidence. This root-only
query is not a structural check, scrub, write probe or durability test. A zero
sickness mask does not mean every inode or allocation group was checked. No
automatic repair, log erasure, reformatting or ownership takeover is enabled.

## Resource limits and persistent leases

Application-path operations run in a private mode of the driver executable,
before API initialization, with a sanitized environment. Each helper has a
three-second deadline; caller cancellation can end the RPC sooner. Input is
limited to 32 KiB and output buffers to 8 KiB. Path resolution precedes observation
for symlinked paths and devtmpfs/tmpfs mounts. Each phase has its own helper
deadline and uses the same admission rules.

At most four probes run per host state directory, with one per volume ID. Kernel
flock leases live under `/var/lib/tns-csi/health-probes`, on the existing persistent
host mount. Both the helper and nested `xfs_io` inherit the leases. A timeout
requests process-group termination, but an uninterruptible task may remain in
the kernel. Its inherited lease continues to block replacement probes, including
after node-plugin restart. RPC completion is not evidence that the child exited.

Keep that host directory stable across plugin restarts and upgrades. Do not
unlink or replace lease files to clear an occupied slot; doing so can admit
another probe while the old task remains. Inspect persistent tasks and handle
the underlying storage failure through the deployment's existing procedures.

Offline fixtures exercise deadlines, inherited leases, restart admission,
diagnostic parsing and identity changes. Genuine D-state faults, forced-shutdown
XFS, actual image/tool compatibility, NAS behavior, deployed health consumption
and fencing still require isolated runtime qualification. Read-only observations
do not establish writable service, durable data or exclusive writer ownership.
