# TrueNAS authentication

An optional API key owner username selects `auth.login_ex` with `API_KEY_PLAIN`.
Initial login, recovery of an expired WebSocket session and reconnect use the
same method and credentials. With an absent or whitespace-only username, the
client retains `auth.login_with_api_key`. Existing URL/key-only Secrets and
three-argument Go constructor calls continue to work where the server offers
legacy authentication. No version probe or automatic authentication downgrade
is performed.

## Configuration

| Client | Optional owner configuration |
| --- | --- |
| CSI driver | `--api-username` or `TNS_USERNAME`; a nonblank trimmed flag wins over the environment. |
| Helm managed Secret | `truenas.username`; empty by default. The chart emits a Secret key only for a nonblank value. |
| Helm existing Secret | Add optional `username` beside existing `url` and `api-key` keys. `truenas.existingSecret` owns all three values; chart credential values are ignored. |
| kubectl plugin | Optional `username` in a consulted explicit/discovered Secret, then `TRUENAS_USERNAME`. Existing URL/key source precedence is preserved. |
| Go library | `tnsapi.NewClient(url, key, skipTLSVerify, tnsapi.WithUsername(owner))`; the positional signature is unchanged. |

For example, a Helm values file can reference an existing credential Secret:

```yaml
truenas:
  existingSecret: truenas-api
  skipTLSVerify: false
```

That Secret supplies `url`, `api-key` and, when modern login is desired, the key
owner's `username`. Both controller and node workloads use an optional username
reference, so an older Secret requires no new key. The API key reaches the driver
through `TNS_API_KEY`, without being added to its command line. Prefer existing
Secret management over key values in shell arguments or command history.

For kubectl, complete `--url` and `--api-key` flags retain their no-Kubernetes-
lookup path; `TRUENAS_USERNAME` is still read. A simultaneous `--secret` does not
override that path. With partial flags, nonblank usernames from consulted
explicit Secrets precede discovered Secrets, then the environment. The selected
owner must match the selected key. Secret changes require a pod rollout because
the driver reads environment credentials at process start.

## TLS and server policy

Use verified `wss://` with the correct hostname and a CA trusted by the client
process/container. Existing `skipTLSVerify` behavior and defaults are preserved;
the CSI chart defaults to verification, while the kubectl plugin retains its
existing default. A TLS verification failure never selects a less secure mode.

The request uses only `login_options.user_info=false`, which is present in the
reviewed [25.10 schema](https://github.com/truenas/middleware/blob/c832dd144f981faa8290c0d243b8dc78ce1925ce/src/middlewared/middlewared/api/v25_10_0/auth.py)
and [27-RC.1 schema](https://github.com/truenas/middleware/blob/d83c3bf89aa4226c8fd1ef25dd273bdf447eb777/src/middlewared/middlewared/api/v27_0_0/auth.py).
No reconnect token is requested or reused. SCRAM, OTP and redirects are not
implemented: policies requiring those mechanisms fail with a safe error instead
of changing server policy or trying legacy login. Server release and requested
API version both matter; older API schemas can normalize newer denial results.

## Failures and recovery

Only a legacy boolean `true` or modern object with `response_type="SUCCESS"`
authenticates. Rejected, expired, denied, malformed or unsupported login responses
produce sanitized errors. A missing legacy method asks for the key owner's
username; an unavailable modern method asks for the server/API configuration to
be checked. There is no fallback after explicit modern configuration.

Initial permanent login failures stop initialization. A permanent auth failure
during session recovery, reconnect or reinitialization stops that client and releases waiters;
credential updates require restarting it. Bounded transport recovery and existing
session-recovery coordination remain in place. Authentication failures do not
disconnect NVMe devices or trigger filesystem recovery.

A successful login does not grant every API role. A permission error on a later
query/mutation is returned as that RPC's error, without repeated login or broader
privileges. Lost responses to non-idempotent storage mutations still return
`ErrUncertainOutcome`; reconnect must not blindly resend them.

Authentication payloads, server error detail and peer close reasons are excluded
from transport logs and authentication error chains, including debug logging.
Common response logging retains correlation metadata rather than entire bodies.

## Upgrade and rollback

Test old and new credential configurations on the exact supported server builds.
Removing the username restores legacy selection only while the server still
supports the legacy endpoint. An old driver cannot be assumed compatible after
server-side legacy removal; retain a tested compatible client/server combination.
No PV or StorageClass migration is required for the auth setting.

Synthetic unit/race/render tests exercise this contract. Real TrueNAS release,
role, session-invalidation and upgrade cells require separate disposable-lab
qualification; source/schema presence does not establish those runtime results.
