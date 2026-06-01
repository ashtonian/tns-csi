# RFC: In-driver auto-recovery for block-volume filesystem shutdowns

**Status:** Draft / proposed
**Component:** `pkg/driver` (node plugin), `pkg/fsrepair`

---

## 1. Problem

When an NVMe-oF or iSCSI target on TrueNAS goes through a transport interruption — a PVE host
reboot, a target VM restart, a network blip mid-write — the Linux initiator occasionally fails an
in-memory metadata consistency check and shuts the filesystem down:

```
XFS (nvme0n1): Corruption of in-memory data (0x8) detected at xfs_trans_cancel+...
EXT4-fs error (device nvme0n1): ... Remounting filesystem read-only
```

On this deployment these events are **~100% transport-induced**, not genuine on-disk damage: the
on-disk metadata is intact, the in-memory state diverged during the disrupted write window. A
fresh mount (kernel log replay) clears them in almost every case. Forced `xfs_repair -n` runs have
consistently reported zero structural inconsistencies.

The cost is operator toil: each event leaves the consuming pods stuck in `ContainerCreating` /
`Init:0/N` until a human notices, and — for the XFS `CORRUPT (0x8)` case specifically — manually
intervenes. There is currently **no filesystem-repair logic anywhere in the driver**.

### Why in-driver, and not an out-of-band agent

Two out-of-band designs were considered and rejected:

1. **A separate privileged DaemonSet** that watches `dmesg`, infers the device→PV→PVC→pod chain from
   `/proc/1/mountinfo` plus the Kubernetes API, and deletes pods to force a reschedule. It is blind
   to the driver's own state (it re-derives everything the node plugin already knows), and it can
   only reschedule around a shut-down filesystem, not repair one.
2. **Reaching into TrueNAS over SSH** (or `system.shell`) to run `xfs_repair` against the zvol. This
   is the wrong shape for two reasons:
   - **It crosses a boundary the driver already owns and the project explicitly avoids.** tns-csi's
     stated design is *WebSocket API only, no SSH* (see `docs/COMPARISON-DEMOCRATIC-CSI.md`). Adding
     an SSH key + shell-exec on the NAS introduces a new credential, a new trust boundary, and
     destructive shell access on the storage host.
   - **It is unnecessary.** `xfs_repair`/`e2fsck` operate on block-device bytes. The initiator
     device `/dev/nvme0n1` on the node holds the *same on-disk metadata* as the backing zvol, so
     there is no reason to go to the NAS to repair it.

Doing the recovery in the node plugin avoids both: it already owns the device, the device→volume
mapping, the TrueNAS WebSocket client, and the unmounted window CSI `NodeStageVolume` provides.

## 2. Goals / non-goals

**Goals**

- Recover transport-induced filesystem shutdowns automatically, on the node, for **both filesystem
  families the driver supports: XFS and ext4/ext3** (`mkfs` switch in `pkg/driver/node_device.go`).
- Cross **no new boundary**: no SSH, no `system.shell`, no new secret. Reuse the TrueNAS WebSocket
  client the driver already authenticates with, and only for an optional snapshot.
- Be **opt-in and default-off**: the driver must start and behave identically with zero overrides.
- Be **safe by construction**: the worst case degrades to exactly today's behavior (filesystem left
  unmounted, human notified) while destroying nothing.
- Be **hard to fool**: a false "corruption" signal must never cause a destructive write or a wrong
  pod eviction.
- Be **self-contained in the driver** — no separate recovery DaemonSet or sidecar.

**Non-goals**

- Recovering genuine on-disk corruption automatically. That stays a human decision (read the
  `xfs_repair -n` output, choose log-zeroing vs restore-from-snapshot).
- NFS/SMB (file protocols): there is no initiator-side `fsck` concept; unaffected.
- Coordinated multi-volume repair. Each device is handled independently under a per-node rate limit.

## 3. Why in-driver is the right home

The CSI node plugin already sits on the right side of every boundary this needs:

| Capability the recovery needs | Already in the node plugin |
|---|---|
| Run `xfs_repair`/`e2fsck` on the device | `privileged`, `hostPID`, `/dev` mounted; `xfsprogs` + `e2fsprogs` already in the image (`Dockerfile`) |
| Know device ↔ volume ↔ fsType | the plugin performs the `nvme connect` and the `mount` in `NodeStageVolume` |
| Exclusive, unmounted access to the device | at `NodeStageVolume` the device is connected but not yet mounted — exactly what `xfs_repair` requires |
| Single-writer guarantee | block volumes are single-node by invariant (`validateAccessModeForProtocol` rejects multi-node mount mode) — no other node holds the device |
| Snapshot the zvol | the existing `apiClient.CreateSnapshot` over the WebSocket the driver already uses |
| Evict the consuming pod | a node-side in-cluster client (the `pkg/dashboard` in-cluster pattern already exists) |

An out-of-band approach would have to *manufacture* this exclusivity (e.g. by detaching the
namespace on the target); the CSI access-mode invariant gives it to us for free.

## 4. Design

Two cooperating halves, both node-side, both in the driver binary, both gated by one master switch.

### 4.1 Repair-on-stage (in-band, CSI-compliant)

Hook the `mount` failure in the shared block path (`formatAndMountNVMeDevice` in
`node_nvmeof.go`, `formatAndMountISCSIDevice` in `node_iscsi.go`; both already do
`verifyDeviceSize → handleDeviceFormatting → mount`). On mount failure, call a shared
`recoverAndRetryMount`, which runs a **cheapest-first** state machine:

1. **Gate.** Proceed only if recovery is enabled for this volume, the device exists and is a block
   device, `blkid` shows a real filesystem matching the expected fsType, and the failure looks like
   a shutdown/structure problem (mount stderr + recent kernel signature) — **not** a missing device
   or the existing size-mismatch refusal (which stays a hard error).
2. **Retry mount.** A fresh mount replays the log; this clears the ~100% transport-induced case.
3. **Read-only check.** `xfs_repair -n` / `e2fsck -fn`. **If clean → stop and do not mutate** — this
   is the central false-positive guard: a wrong "corrupt" signal dead-ends here harmlessly.
4. **Non-destructive repair** (opt-in `repair`): `xfs_repair` (clean log) / `e2fsck -p` (preen).
   Optionally snapshot the zvol first.
5. **Destructive repair** (separate opt-in `repairDestructive`): `xfs_repair -L` (zeroes the log) /
   `e2fsck -fy`. Only reached if non-destructive repair reports it is blocked. Snapshot first.
6. **Retry mount**, emit a Kubernetes Event on the PVC with the signature, the `-n` output, the
   action taken, and the duration.

A **per-device failure limiter** (max *failed* attempts per window) bounds repeated attempts; once
the budget is exhausted, recovery stops and surfaces to a human. A successful recovery resets it.

### 4.2 Mid-flight reconciler (out-of-band node agent)

A goroutine started from `driver.Run()` (node mode, gated), cancelled in `Stop()` — the same
pattern the metrics and dashboard servers already use. It runs three loops:

- **Shutdown watcher**: reads `/dev/kmsg` for XFS *and* ext4 shutdown signatures, correlates the
  device to a staged volume via the authoritative tracker (§4.3), and — for a confirmed shutdown of
  an already-mounted volume — evicts the consuming pod so kubelet reschedules → repair-on-stage
  runs.
- **Stale globalmount sweeper**: when a staged device is gone, lazy-unmount the stale globalmount so
  kubelet can recreate it. (Ungraceful device loss never triggers `NodeUnstageVolume`; this is a
  genuine kubelet blind spot.)
- **Stale bind healer**: when a pod's bind mount returns EIO while its globalmount is healthy, evict
  the pod so its controller recreates it with a fresh bind mount.

### 4.3 Authoritative mount tracker (the false-positive foundation)

`mountTracker` records, at `NodeStageVolume`, `{stagingPath → volumeID, devicePath, fsType,
protocol, autoRepair-policy}`, clears it at `NodeUnstageVolume`, and persists it as JSON on the
host-backed `/var/lib/tns-csi` volume (a fixed hostPath in the node DaemonSet), reloading it on
startup so it survives a plugin pod restart while host mounts persist. The PVC reference is resolved
on demand via the node's Kubernetes client (PV `volumeHandle` → `claimRef`), not stored. The
reconciler acts **only** on volumes in this map — authoritative knowledge of what the node staged,
rather than re-deriving it by scraping `/proc`. (The sweep/heal loops additionally
read `/proc/1/mounts` to locate the live mounts for tracked volumes.)

## 5. Configuration

All defaults are off/safe — the driver starts identically with zero overrides. Each value is a
driver flag (global default), overridable per-StorageClass via a parameter that the controller
surfaces into `volumeContext` (so the node needs no extra API call at stage time).

| Flag / SC parameter | Type | Default | Description |
|---|---|---|---|
| `--auto-recovery` | `off\|shadow\|on` | `off` | Master switch. `shadow` = detect, log, and emit Events but take **no action** (false-positive validation). `on` = act. |
| `--auto-recovery-repair` | bool | `false` | Allow non-destructive repair at stage time (`xfs_repair` clean-log / `e2fsck -p`) after the read-only check confirms inconsistencies. |
| `--auto-recovery-repair-destructive` | bool | `false` | Separate gate for the data-losing last resort (`xfs_repair -L` / `e2fsck -fy`). |
| `--auto-recovery-snapshot` | bool | `true` | Take a ZFS snapshot (existing `apiClient.CreateSnapshot`) before any **mutating** repair. Disengageable. |
| `--auto-recovery-evict` | `evict\|delete` | `evict` | Reconciler uses the Eviction API (respects PodDisruptionBudgets) vs raw delete. |
| `--auto-recovery-debounce` | int | `3` | Consecutive confirmations required before the reconciler evicts/repairs. |
| `--auto-recovery-cooldown` | duration | `300s` | Per-device cooldown between actions. |
| `--auto-recovery-max-evictions` | int | `5` | Max evictions per node per window. |
| `--auto-recovery-repair-timeout` | duration | `10m` | Hard bound on a single repair invocation. |
| `--auto-recovery-retry-window` / `-retries` | duration / int | `1h` / `3` | Per-device failure-limiter window and attempt cap. |
| SC param `recovery.autoRepair` | `"true"`/`"false"` | unset → global default | Per-StorageClass opt-out/opt-in surfaced into volume context; `"false"` opts a volume out. |
| ConfigMap `tns-csi-recovery` key `enabled` | bool | absent → enabled | Cluster-wide kill switch, watched by the reconciler (pause without redeploy). |

## 6. Security & RBAC

No new TrueNAS surface: the only TrueNAS call is the optional snapshot, over the WebSocket client
the driver already authenticates with. No SSH, no `system.shell`, no new secret, no node-condition
writer.

The blast-radius delta is on the **node** ServiceAccount (`-node-role`), which today has only
`nodes:get` + `events:*`. It gains:

| API group | Resource | Verbs | Why |
|---|---|---|---|
| `""` | `pods` | `get`, `list` | find the pod consuming an affected PVC on this node |
| `""` | `pods/eviction` | `create` | evict it (PodDisruptionBudget-respecting; safer than raw delete) |
| `""` | `persistentvolumes` | `get`, `list` | device → PV → PVC resolution |
| `""` | `persistentvolumeclaims` | `get`, `list` | Event target + per-PVC policy |
| `""` | `configmaps` | `get`, `list`, `watch` | cluster kill switch + policy |

The controller role is unchanged. Using `pods/eviction` rather than a raw `pods` `delete` is
deliberate: the Eviction API respects PodDisruptionBudgets.

## 7. False-positive strategy

The worst case must destroy nothing. Layered defenses:

1. **Correlation over inference.** Act only on volumes in the authoritative `mountTracker`; ignore
   any device/mount not ours.
2. **Independent confirmation before any mutation.** The read-only check (`-n` / `-fn`) gates every
   destructive op; a clean result stops recovery and returns the original error.
3. **Cheapest-first.** Retry mount → read-only check → non-destructive repair → destructive (only
   behind its own flag).
4. **Debounce + cooldown** before eviction/repair — kills transient reconnect-window EIO.
5. **Failure limiter + rate limits + kill switch** — a false-positive storm is bounded; on budget
   exhaustion, stop and surface.
6. **Real-read health probe** distinguishes "mount present but dead" from healthy; confirms the
   globalmount is healthy before blaming a bind mount; only evicts Running, non-terminating pods.
7. **Shadow mode** runs the full decision logic and emits the action it *would* take, without taking
   it — validates against reality before enabling.

## 8. Rollout

- **Phase A — dormant foundation:** `pkg/fsrepair` + signatures + `mountTracker` + config + node
  client. Default `off`; no behavior change. Unit tests.
- **Phase B — repair-on-stage:** wire `recoverAndRetryMount` into both block mount-failure branches.
- **Phase C — reconciler:** shutdown watcher + sweeper + bind healer, with shadow mode.
- **Phase D — chart + docs:** node RBAC, flags, values, docs. Recommended operator rollout: run
  `shadow` for 2–3 weeks and eyeball the Events/logs, then flip `on` per-StorageClass, then
  node-global once confidence is established.

## 9. Testing

- **Unit (`-race`, table-driven):** repairer command construction + exit-code/output classification
  per fsType (real `xfs_repair -n` / `e2fsck` fixtures); signature parsing (XFS + ext4 kmsg + mount
  stderr); `mountTracker` stage/unstage/rebuild; failure-limiter/debounce state machine.
- **Integration (real deps):** `losetup` + real `mkfs.xfs`/`mkfs.ext4`; force a shutdown with
  `xfs_io -c shutdown` or a `dmsetup` error target; assert a clean check does **not** mutate and a
  non-destructive repair + remount succeeds.
- **E2E (Ginkgo):** extend `tests/e2e/nvmeof` — stage a volume, simulate a transport drop, assert
  automatic recovery with `--auto-recovery=on`.
- **Shadow validation:** deploy `shadow`, induce a drop, confirm Events show the correct decision
  before enabling `on`.

## 10. Code map

| Path | Role |
|---|---|
| `pkg/fsrepair/fsrepair.go` | `Repairer` interface; `xfsRepairer`, `extRepairer`; `Outcome` classification |
| `pkg/fsrepair/signatures.go` | parse XFS/ext4 kmsg + mount-stderr shutdown signatures |
| `pkg/driver/recovery.go` | `recoverAndRetryMount` state machine, snapshot, Event, failure limiter |
| `pkg/driver/recovery_tracker.go` | authoritative `mountTracker` |
| `pkg/driver/recovery_reconciler.go` | node agent: shutdown watcher, sweeper, bind healer |
| `pkg/driver/kube_node.go` | node in-cluster client: eviction, PV/PVC lookup, Events, kill switch |
| `cmd/tns-csi-driver/main.go`, `pkg/driver/driver.go`, `node.go` | flags, config, lifecycle, tracker hooks |
| `node_nvmeof.go`, `node_iscsi.go`, `controller.go` | mount-failure hook; surface SC params into `volumeContext` |
| `charts/tns-csi-driver/*` | node RBAC, flags, `recovery:` values, docs |

## 11. Out of scope

- Genuine on-disk corruption recovery (stays manual; destructive flag default off).
- NFS/SMB.
- Coordinated multi-volume repair (independent per device under the per-node rate limit).
