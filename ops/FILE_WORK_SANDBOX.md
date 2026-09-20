# Candidate file-work launcher

`work_sandbox.py` is a disabled-by-default candidate boundary, not a deployment
or model-auth setup. Invoke it as the configured model UID:

```text
python3 /approved/path/work_sandbox.py --config /protected/sandbox.json --work-root /protected/works/one-work -- /approved/native-cli [arguments]
```

The adapter must validate the launcher and configuration before enabling its
file-work capability. The JSON has exactly these fields:

```json
{
  "enabled": false,
  "work_base_dir": "/protected/works",
  "action_tools_socket": "/protected/action-tools.sock",
  "command": "/approved/native-cli"
}
```

The config, its parent, base, work root and inputs belong to the host authority
and cannot be group/world writable. A non-root model UID cannot own its config.
The work root is a direct child of the base. The gateway owns the base (`0750`),
work root/inputs (`02750`) and outputs directory (`02770`); their group is the
model account's primary group. Only the gateway and that model may use this group.
Provision gateway membership explicitly before enabling; startup checks it and
never changes permissions. Ordinary gateway creation uses group inheritance or
`chgrp` to an existing membership, never cross-UID `chown` or DAC capabilities.
Input files are `0440`. Generated output files belong to the executing model UID.
Paths must be absolute and contain no symlink components. The native
command is an existing root-owned executable, fixed by the config. The host
must bind `action_tools_socket` to the existing restricted `ActionToolsHandler`
only; never mount the management API socket there. This script creates no proxy.

The launcher creates user, mount, PID, network, IPC and UTS namespaces, a fresh
root filesystem and private `/proc`, HOME and temporary storage. `pivot_root`
and detaching the old root prevent traversal back to the host. The only host
work mounts are this work's read-only inputs and writable outputs, at their
original absolute paths. The fixed OS runtime directories `/usr/bin`, `/usr/lib`
and existing lib variants are read-only; these must contain only trusted OS
runtime files, never business configuration or credentials. A configured native
CLI outside these directories is mounted as one read-only file. No host `/etc`,
HOME, work base, `/var`, `/run` or temporary directory is mounted.

The adjacent protected `file_tool.py` is the sole additional client artifact.
The launcher pins and reads that file, copies exactly those bytes to the fresh
root as `/usr/local/bin/cc-connect-file`, verifies the SHA-256 and makes the copy
executable before sealing the root read-only. That directory joins the private
PATH. The host `/usr/local` is never mounted, and other files alongside the
launcher are not exposed.

`file-deliver` publishes only the selected model-owned, single-link file to the
host group (`0640`), and selected model-owned parent directories (`02750`). It
checks a no-follow descriptor and digest first; unchanged private drafts stay
private. The host independently validates and snapshots the bytes as before.
The work mount boundary, not a shared group alone, prevents cross-work access.
The private host ledger and snapshots remain outside every model mount. Existing
old-layout work directories are refused, never automatically migrated/reowned.

Only `CC_FILE_ACTION_TOKEN` and `MYANC_CRM_ACTION_TOKEN` may be inherited; both
are session capabilities, not provider credentials. The launcher supplies
`CC_ACTION_TOOLS_SOCKET=/run/cc-connect/action-tools.sock` for HTTP over Unix.
All capabilities are dropped and `no_new_privs` is set before the command runs.
The command must communicate over pipes; do not supply host directory or secret
file descriptors through its standard streams. Supervisor death terminates the
namespace. Work inputs/outputs must remain host-admitted regular files without
symlinks, hardlinks or special objects; model processes have no host paths from
which to create cross-work hardlinks.

Python 3 (standard library), existing util-linux `unshare`/`setpriv`, and Linux
x86-64 or arm64 are required. Unsupported systems, missing configuration or
failed namespace creation refuse the work; there is no sudo, unsandboxed, other
model or network fallback. The operator must separately prove that the actual
model UID can create these namespaces on the intended host.

Run admission tests with `python3 -B ops/test_work_sandbox.py`. The required
Linux boundary test uses only synthetic files, a Python probe, a local fake
HTTP-over-Unix `/tool` socket and a local host listener. In a disposable CI
runner only, use existing sudo:

```sh
sudo -n python3 -B ops/test_work_sandbox.py --require-isolation
```

The CI also records whether unprivileged namespace creation is available.
A privileged fixture pass does not prove runtime UID readiness. It does not
authorize host configuration changes or deployment.

The additional `file_handoff_fixture.py` CI initializer provisions two temporary
ordinary users and a single model-target sudo rule, then runs the real Engine
file journey as the gateway user. Each synthetic model turn uses the production
launcher and client under the other UID, with zero model capabilities. The test
covers XLSX processing, two reply revisions, a new work and a DOCX with a selected
fictional reference; the host reads/snapshots/sends to a fake receipt sender.
Other work, ledger, snapshots and admin paths must be hidden; `/send` is denied.
Controlled setup temporarily lifts the hosted Ubuntu runner's AppArmor userns
restriction when present and restores it afterwards. It never runs a model or
connects to Feishu. This proves the code with those stated Linux prerequisites,
not readiness of an unchanged production service namespace or security policy.

Provider auth and egress,
approved runtime Skill/tool bundles, and knowledge client Unix transport are
not connected or accepted in this candidate. Keep real model file work disabled
until those requirements and the actual runtime UID boundary are accepted.
