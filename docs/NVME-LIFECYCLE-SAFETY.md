# NVMe connection and queue policy

Failed discovery, capacity checks, connection attempts, staging or caller
cancellation retain the NVMe connection. A later CSI retry can observe it again.
Only explicit unstage disconnects a subsystem, after checking its exact NQN,
staging source and observed namespace users. Missing or unreadable identity is
an error, never proof that a controller disappeared. The plain-text staging NQN
sidecar remains compatible with previous versions.

Unstage depends on completed CSI unpublish ordering. A negative mount scan does
not exclude arbitrary external mounts or open raw-device handles and does not
provide fencing. Intentional raw-block RWX remains supported. There is no
automatic repair, log erasure, formatting of uncertain media, or ownership takeover
in this change.

## Optional parameters

Set explicit parameters on a new NVMe StorageClass, or use a scoped node profile
for existing immutable PVs. Omitted settings retain the existing behavior.
Numbers in the new fields must contain decimal ASCII digits only; empty values,
signs, whitespace, units, fractions and overflow are rejected.

| Parameter | When omitted | Accepted new value |
| --- | --- | --- |
| `nvmeof.reconnect-delay` | 2 seconds | 1–2147483 seconds |
| `nvmeof.ctrl-loss-tmo` | 60 seconds | 1–2147483647 seconds |
| `nvmeof.keep-alive-tmo` | 5-second timeout | 1–2147483 seconds |
| `nvmeof.nr-io-queues` | Request 4 | Existing PV strings keep their previous CLI interpretation; profiles require positive decimal int32 |
| `nvmeof.queue-size` | Omit the CLI argument | Existing PV strings keep their previous CLI interpretation; profiles require decimal 2–1024 |
| `nvmeof.max-sectors-kb` | Leave unchanged | Positive KiB, at most 9007199254740991 |
| `nvmeof.read-ahead-kb` | Leave unchanged | KiB, 0–9007199254740991; explicit 0 disables readahead |

The timeout ceilings bound signed integer conversions and timer products for
kernel configurations with HZ at most 1000. They are parser bounds, not a claim
that every value works with every kernel, nvme-cli version or target. Check the
actual initiator versions before rollout. Queue counts and depth are requests;
the kernel and controller can negotiate smaller values.

For example, a new StorageClass can include:

```yaml
parameters:
  protocol: nvmeof
  pool: tank
  server: 192.0.2.10
  nvmeof.max-sectors-kb: "1024"
  nvmeof.read-ahead-kb: "0"
```

Explicit PV values override matching profile values independently per field.
The built-in defaults fill only the remaining connection settings. Active
connections are never disconnected or retuned to apply connection-only changes;
those changes take effect on the next normal connection creation. Queue policy
can be checked again on safe CSI lifecycle calls.

## Scoped node profile

Supply `--nvmeof-node-profile=/etc/tns-csi/nvmeof-policy.json`, or set
`TNS_NVME_NODE_PROFILE`. An explicitly supplied flag, including an empty flag,
overrides the environment. An empty path performs no profile file I/O. An invalid
supplied file fails startup before creating the NAS API client.

Helm's `node.nvmeofPolicy` accepts the same object:

```yaml
node:
  nvmeofPolicy:
    version: 1
    profiles:
      - name: example-target
        transport: tcp
        server: nas.example.test
        port: 4420
        addresses:
          - 192.0.2.10
          - 2001:db8::10
        nqns:
          - nqn.2024-01.test.example:volume-a
        parameters:
          nvmeof.max-sectors-kb: "1024"
          nvmeof.read-ahead-kb: "128"
```

Only the node DaemonSet receives the ConfigMap, read-only mount, flag and checksum
annotation. Default empty values add none of these resources or arguments.
Profiles are settings, not credentials.

Selectors require exact server, TCP port and, when supplied, exact NQN matches.
DNS names are compared case-insensitively after removing a trailing dot; literal
IPv4/IPv6 addresses are normalized. A numeric server pins itself by default.
Hostname profiles require explicit numeric `addresses`. The first pinned address
is used for a new connection; this release does not try alternate pins on failure.
Connected controller `traddr` and `trsvcid` must match an allowed numeric address
and the selected port. A later DNS change cannot silently authorize a different
data endpoint. Update pins deliberately when changing the target address.

A PV with new explicit policy and a hostname server also needs a matching pinned
profile. Legacy hostname PVs without new policy retain their previous arguments.
An omitted NQN list selects every NQN at that server and port; an explicit empty
list is invalid. Multiple matching profiles may supply disjoint or identical
settings, but conflicting settings or address pins fail startup. Unknown fields,
duplicate JSON keys, unsupported versions and oversized files are rejected.

## Enforcement and restart behavior

Before policy-dependent use, the node verifies the namespace's exact NQN, NSID 1,
device number, sysfs owner and UUID/NGUID/EUI when available. For native multipath,
all visible head/path queues and controller endpoints must agree with that scope.
Local PCIe devices, other transports and other NQNs never receive scoped writes.
Identity and path membership are checked again around writes. A pinned sysfs
file descriptor prevents a replaced device name from redirecting a pending write.

The transfer cap is the minimum of the requested, current and hardware caps,
rounded down to logical-block alignment. A lower observed current cap is never
raised. The node writes the cap before readahead because a cap update can change
readahead, then reads the configured fields back. Unknown identity, invalid queue
state, write failure or failed readback prevents new use. No failure triggers a
reset of an existing consumer.

Checks run before stage probes, after stage/reuse rescans, before publication,
and before and after filesystem expansion. Raw-block expansion verifies policy
before returning its no-filesystem-resize success. Without policy, old volumes
keep their existing behavior.

Explicit policy and endpoint provenance are stored in the optional ZFS user
property `tns-csi:nvmeof_policy`. Retries cannot silently change or omit retained
explicit settings. Clone records bind to the destination volume and NQN rather
than inheriting the source's policy identity. Persistence failure retains created
resources for retry. Old volumes with no property remain valid.

The node also keeps versioned, atomic, mode-0600 records under
`/var/lib/tns-csi/nvmeof`, keyed by a SHA-256 of the unchanged volume ID. A separate
staging marker detects loss of that record. The records contain explicit PV
fields and observed namespace identity; profile values and defaults are resolved
again from the current config. Expansion, which has no VolumeContext, uses these
records after a restart. Keep the chart's persistent `/var/lib/tns-csi` host mount.
Corrupt, unsupported or conflicting records require restoration; they are not
discarded automatically. Records remain after unstage to preserve provenance.

An existing volume with no node record must pass a normal stage/restage before
expansion under a configured profile. This binds the immutable PV endpoint to
the actual namespace instead of guessing from a publish path. Loss of both the
durable directory and staging marker requires restoring host metadata. When the
node record is missing, expansion also checks the remote optional property;
retained remote policy or an unavailable API prevents legacy fallback. Static
volumes without remote metadata depend on the persistent host directory. A rollback to an older
driver does not enforce these records and restores its older recovery behavior.

## Validation boundary

Offline tests use synthetic sysfs, mount tables, command seams and NAS API mocks.
They do not prove runtime transfer size, durability, arbitrary-writer exclusion,
kernel reconnect behavior or exact-image integration. Linux/NAS tests must qualify
the deployed kernel/nvme-cli versions, namespace recreation, multipath recovery,
request-size observations and checksum/fsync behavior before rollout.

Kernel reconnects and external queue recalculation do not always invoke CSI.
Keep existing udev and periodic I/O guards until add/change/reconnect coverage
demonstrates a permanent enforcement owner. Do not disconnect an active volume
to change settings or to roll back.

Source contracts: [Linux v6.12 fabrics parsing](https://github.com/torvalds/linux/blob/v6.12/drivers/nvme/host/fabrics.c),
[nvme-cli v2.13 option parsing](https://github.com/linux-nvme/nvme-cli/blob/v2.13/util/argconfig.c),
and [Linux hidden-disk registration](https://github.com/torvalds/linux/blob/v6.12/block/genhd.c).
