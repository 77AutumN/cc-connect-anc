# Controlled file work candidate

Disabled by default. This candidate does not authorize deployment, account or
permission changes, model execution, or live Feishu sends. The offline launcher
has no provider network/authentication; real model readiness is **incomplete**.

An enabled project uses one fixed sender/chat Feishu route and a distinct existing
`run_as_user`. Each directed top-level request starts a native session and work.
Replying to that request or a returned file resumes that exact work. An unknown
reply is rejected; an unmentioned group message is accepted only with a recorded
parent from the same authenticated principal. New work never borrows recent inputs.
Concurrent requests receive the existing busy response; no background queue is added.

Inputs are limited to four files, 20 MiB each and 40 MiB total, with a 30-second
download deadline. The first format boundary accepts local DOCX/XLSX packages;
encrypted, macro/active content and external relationships are rejected. Originals
are retained with hashes. Output files must be in this work's `outputs`, regular,
single-link, owned by the configured model UID, and at most 20 MiB (100 MiB expanded).
One checked descriptor supplies the immutable snapshot and bytes actually sent.

Only `work-context`, `file-deliver`, `file-status` are added to the existing `/tool`
handler. They take the live host session token; identity, destination and work
authority are never caller-selected. The Unix endpoint has no management routes.
`ops/file_tool.py` is installed as `cc-connect-file` only during separately
authorized runtime provisioning. JSON comes from stdin, not a shell argument.

Delivery versions use compare-and-set. The host durably records `submitted` and
the delivery UUID before the single network attempt. Accepted requires a Feishu
message receipt. Missing/uncertain results stay `unknown` and block resends,
including renamed copies. Restart converts incomplete submissions to unknown.
An API acceptance does not prove an employee received or opened the file.

Example configuration (all paths are fictional; do not apply to a live host):

```toml
[projects.file_work]
enabled = false
ledger = "/srv/file-host/project/ledger/files.db"
snapshots = "/srv/file-host/project/snapshots"
work_base_dir = "/srv/file-host/project/works"
tools_socket = "/srv/file-host/project/tools/action-tools.sock"
# Under the existing projects.agent.options table:
# file_work_launcher = "/opt/file-candidate/work_sandbox.py"
# file_work_config = "/srv/file-host/project/runtime.json"
# cmd must name the protected native CLI executable; legacy wrappers are not used.
```

The ledger/snapshot/tool parents and work base must already exist under protected
host ownership. `cc-connect files-init /absolute/protected/files.db` is an explicit
operator-only initialization and refuses an existing file; startup never initializes
or resets a ledger. Only new session directories are created by the host. Existing
directory ownership/modes are checked rather than repaired. A missing, replaced or
corrupt ledger fails closed. Do not restore an older ledger to clear unknown sends.

See [the isolation boundary](FILE_WORK_SANDBOX.md). The candidate does not install
CLI, document libraries, Skills, credentials, sudoers or ACLs. Provider access,
approved policy/Skill visibility, and knowledge client access inside the namespace
remain runtime acceptance work before enabling real models. Reverting source and
keeping `enabled = false` rolls back capability exposure; preserve the ledger and
snapshots for reconciliation, without replaying any send or business operation.
