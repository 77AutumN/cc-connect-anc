# CRM follow-up ActionHost

This fork contains one deliberately narrow host-owned action adapter,
`crm.followup.v1`. Claude may stage a CRM follow-up, but only cc-connect can
bind the staged change to an authenticated Feishu sender and apply it. The CRM
helper owns the SQLite ledger, compare-and-swap state transitions, Feishu
writes, re-reads, and receipts. cc-connect does not contain a workflow engine.

## Selected development path — rollout paused

The general assistant keeps one ongoing Claude session. CRM is one bounded tool
family: `customer` reads facts/history, `stage` prepares a canonical follow-up,
`assignee` resolves a person, `stage-customer-create` and `stage-customer-update`
prepare customer profile proposals, and `result` queries the stored actual state.
A session bearer resolves to the Engine's live
Principal; no model field supplies identity or authorizes a write.

Set host-only deployment option `MYANC_CRM_TOOLS=1` to explicitly opt in;
otherwise the legacy path is unchanged. Invalid values, missing host credentials,
or duplicate target projects fail startup. Tests may call `Adapter.EnableTools()`
before `SetWorkDir()`. This selects the controlled
`Engine.ActionToolHandler()` path: token only, no inbox, no marker matching or
PermissionRequest interception. The stage response supplies both the model's
facts and the host's card. Approve/modify/cancel still use the existing durable
bind/claim/execute protocol below; clicking does not wake or terminate Claude.

The fixed 2.1.259 native proxy passed all 16 isolated transport checks after
authorized re-login. `myanc-crm/ops/crm_tool.py` now uses that single HTTP path;
the earlier Unix implementation/duplicate experiment are not runtime options.
Keep sandbox enabled with no unsandboxed fallback or excluded command; permit
only `127.0.0.1:18743`, not bare localhost or all Unix sockets. The client removes
`NO_PROXY`/`no_proxy` only locally, retains proxy authentication, and refuses
redirects, external proxies, direct fallback and retries. Do not route loopback
through an external parent proxy.

`main` binds the dedicated IPv4 loopback listener only for the explicitly enabled
project, before receiving messages. An occupied port does not trigger a fallback.
Only `Engine.ActionToolHandler()` is installed, never cron/relay/admin routes.
The main process closes the listener on shutdown and shuts down on an unexpected
listener exit; ledger outcomes remain authoritative for interrupted requests.
Host secrets remain outside Claude's environment. The session bearer cannot approve.

On native `/restart`, only after engines stop, the captured host secret (or its
original protected file path) is supplied explicitly to the new cc-connect
process. It is not restored in the parent's global environment. The new process
captures and clears it again before any agent or helper is created.

Do not activate a deployment from these notes. Runtime-user permissions, real
callback validation and unrelated Feishu resource isolation remain rollout gates.
Automatic merge/deploy is paused.
See the CRM repository's `CLAUDE.md` for the current model-facing contract.

## Legacy startup reference — not the selected tools path

Build the Linux binary without the embedded web UI when the web assets are not
present:

```bash
go build -tags no_web -o cc-connect ./cmd/cc-connect
```

Set these service variables before starting cc-connect:

```text
MYANC_CRM_PROJECT=<exact [[projects]].name>
MYANC_CRM_HOST_COMMAND=/usr/local/bin/crm-followup
MYANC_CRM_HOST_SECRET=<random 32..4096 byte value>
```

Instead of the last variable, `MYANC_CRM_HOST_SECRET_FILE` may name a
service-readable secret file with mode `0600`. Do not set both. The helper path must be absolute and
the production helper should be root-owned and not writable by the Claude
runtime user. `NewFromEnv` captures the secret and removes both secret variables
from the daemon environment before any Agent is created. The Claude launcher
also strips them after all project/session env merging. Only the short-lived
helper subprocess receives the raw secret.

The target project must keep these existing settings:

```toml
[[projects]]
name = "<same value as MYANC_CRM_PROJECT>"
run_as_user = "<non-privileged Claude user>"

[projects.agent]
type = "claudecode"

[projects.agent.options]
work_dir = "/home/<user>/ClaudeRemote"
mode = "auto"

[[projects.platforms]]
type = "feishu"

[projects.platforms.options]
enable_feishu_card = true
share_session_in_channel = false
thread_isolation = false
```

The service may load the variables from a root-only systemd
`EnvironmentFile`. Keep the existing `ExecStart` shape and replace only the
binary path, for example:

```ini
[Service]
EnvironmentFile=/etc/cc-connect/crm-action-host.env
ExecStart=/usr/local/bin/cc-connect
```

## Legacy request-file protocol

For each new live Claude session, cc-connect generates and injects
`MYANC_CRM_ACTION_TOKEN` plus `MYANC_CRM_STAGE_INPUT`. Production legacy mode uses
the fixed host inbox `/var/lib/cc-action-inbox/<sha256(action-token)>.json`; direct
unit-test fixtures may use workspace-local `.crm-input`. Provision the host
exchange directory as `ccconnect:cc-action` with mode `3730` (setgid + sticky,
owner `rwx`, group `wx`, no access for others), and add the configured
`run_as_user` and the `ccconnect` service user to `cc-action`. cc-connect only
validates the pre-provisioned directory; it never creates, chmods, or chowns it.
The stage command writes one
`claude:cc-action` regular file with mode `0640` to that exact path. Both staging variables are
automatically preserved across `run_as_user`; administrators must not add either
host-secret variable to `run_as_env`. Claude's only approval handoff is this
exact argv:

```text
/usr/local/bin/crm-followup request-approval
```

Aliases, other paths, whitespace changes, and extra arguments do not match the
handoff.

On the matching PermissionRequest, cc-connect invokes:

```text
crm-followup host-prepare
stdin: {"request":{...untrusted staged request...},"principal":{"platform":"feishu","user_id":"...","chat_id":"...","session_key":"...","project":"...","message_id":"..."}}
env:   MYANC_CRM_ACTION_TOKEN + MYANC_CRM_HOST_SECRET
```

cc-connect opens the token-derived request beneath the fixed inbox root,
requires a regular, non-symlink, single-link file owned by `run_as_user`, with
exact mode `0640` and a maximum size of 64 KiB. It validates that the content
is a JSON object and removes the file before invoking `host-prepare`. The model
cannot select a change ID or helper input path.

Expected JSON includes `status`, `approval_id`, `change_id`, `expires_at`,
canonical `preview`, and `superseded_approval_ids`. cc-connect first denies the
Claude tool request. If that response cannot be delivered, the Agent session is
closed and no approval card is published. After a successful deny, cc-connect:

1. sends a non-actionable placeholder and receives its real Feishu message ID;
2. invokes `host-bind-card` with that ID and the trusted principal;
3. replaces that exact placeholder with the canonical preview and opaque
   approve/modify/cancel buttons.

A publish, bind, or activation failure never exposes approval buttons. The
gateway silently drains the rest of the Agent turn and does not show a generic
Allow/Allow All card.

## Shared card binding and execution

Both paths reuse the following protocol. The send-time binding call is:

```text
crm-followup host-bind-card
stdin: {"approval_id":"apr_...","principal":{"platform":"feishu","user_id":"...","chat_id":"...","session_key":"...","project":"...","message_id":"<emitted-card-id>"}}
env:   MYANC_CRM_HOST_SECRET only
```

Button callbacks first claim and then execute through two separate calls:

```text
crm-followup host-claim
stdin: {"approval_id":"apr_...","decision":"approve|modify|cancel","principal":{"platform":"feishu","user_id":"...","chat_id":"...","session_key":"...","project":"...","message_id":"..."}}
env:   MYANC_CRM_HOST_SECRET only

crm-followup host-execute
stdin: {"approval_id":"apr_...","principal":{"platform":"feishu","user_id":"...","chat_id":"...","session_key":"...","project":"...","message_id":"..."}}
env:   MYANC_CRM_HOST_SECRET only
```

The Feishu callback's OpenID and chat ID are authoritative. The session key is
recomputed from those values and the fixed platform configuration; the value
embedded in the card is routing data only. The helper compares platform, user,
chat, session, project, and the send-time-bound card message ID. `host-claim`
performs the durable decision CAS before the card changes to “executing”; only
an approved claim yields a continuation that calls `host-execute`. A callback
never sends a second user message to Claude. The exact callback message ID is
used to update the original card, so copied or concurrent cards cannot redirect
the write or overwrite another approval.

## Customer profile operation followed by an independently approved follow-up

The adapter kind stays `crm.followup.v1` for compatibility. There is no additional
adapter registry, workflow engine, MCP server or process handoff. New tool
commands use the same bounded HTTP envelope and trusted Principal as the original
three commands. Missing required profile fields return `needs_input`; the model
must ask for them, not silently create a customer or choose an assignee.

`preview.operation_type` distinguishes `customer_create` and `customer_update`.
`effects.customer_profile` contains canonical business fields and old/new changes.
An optional `followup_draft` is displayed as **not approved** and is not one of the
parent's write effects. The card displays only known business fields; opaque
person/record references are never rendered, while the operation ID remains.

Only the parent's first successful `host-execute` may return transient
`next_approval` in the existing pending-approval shape. The adapter exposes one
optional `ActionHostResult.Next`; `Claim`, replay receipts, non-pending results
and nested next approvals never republish a card. The callback continuation:

1. keeps the parent verified receipt;
2. reconstructs reply context from the validated session Principal;
3. publishes a separate non-actionable placeholder, binds its exact new message
   ID and activates the canonical follow-up card using the existing path;
4. requires a new trusted callback to execute the follow-up. It never wakes Claude.

Duplicate callback claims remain governed by the Python durable CAS; duplicate
invocation of the same continuation is also one-shot. A wrong card ID produces
only an error toast, not a replacement of the other operation's receipt.

If publishing, binding or activating the second card fails, the parent remains
verified and its card states that the follow-up was not submitted. A bound message
is not proof that activation succeeded. Query stored operation state before manual
recovery; this code does not retry, reconstruct a stale pending card from a receipt,
or roll back an already completed customer operation. Cancelling, modifying or
expiring the child similarly leaves the parent operation intact, and child cards
state that distinction explicitly.

Offline regression gates include `TestCUJ_CUSTOMER1` in `core/cuj_test.go` plus
`TestCUJ_CRMIPC2` in the CRM adapter. The latter is opt-in with
`MYANC_SPIKE_CRM_ROOT` pointing to the companion checkout and uses its persistent
synthetic Feishu fixture, real Python core/SQLite and real gateway HTTP/callback
path. It covers history, missing profile input, assignee resolution, parent create,
two separate approvals, sender/card mismatch, replay, update/child cancellation,
result queries, Quiet and continued discussion without restarting Claude.
These tests do not prove live model understanding or real Feishu callback wiring;
existing platform tests and a separately authorized canary are still required.

## State and rollback

No ActionHost state is authoritative inside cc-connect. Every decision is
authorized and compare-and-swapped by the helper's protected SQLite ledger,
which must survive daemon restarts and make decisions idempotent.

Rollback is binary-compatible with the upstream v1.5.0 configuration: stop the
service, restore the previous cc-connect binary, and remove the optional
ActionHost settings that were configured (`PROJECT`, `COMMAND`, and the chosen
secret source). Do not roll back already verified Feishu writes by
replacing the ledger. Preserve their receipts and disable the new entry point
instead.
