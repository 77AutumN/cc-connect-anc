# Narrow image-cache release procedure

This is a maintenance checklist, not permission to deploy. Production maintenance time must be separately confirmed by the Owner. Keep the current service and real CRM untouched while preparing/reviewing artifacts.

## Preflight and preservation

- Record the running gateway/CRM/policy hashes, service user/groups, effective umask, mount restrictions, one receiver, and current verified release. Preserve unrelated checkout changes.
- Back up only the binaries, model rule file and exact service override being replaced. Preserve real CRM data and the approval ledger in place; never restore a stale ledger/database backup over business results.
- Inspect each component of `/home/claude/ClaudeRemote/.cc-connect/attachments/images` with `lstat`, owner/group/mode and the actual service mount namespace. Reject any symlink or unexpected existing directory/contents. Do not recursively change permissions or ownership.
- Required topology: workspace and existing control plane unchanged; any newly created traversal parents administrator-owned and traversable by the already-established `cc-action` group, with no new gateway write permission on those parents. Final `images` directory owned by `ccconnect:cc-action`, mode `02750`; original files `0640`, temporary files initially `0600`. Claude remains read-only via the existing group. If existing parents conflict with these requirements, stop for a narrower reviewed provision rather than widening the workspace.
- If systemd mount restrictions require a writable exception, add only the exact final cache directory to the reviewed `ReadWritePaths` override. Do not change `UMask`, global workspace allowances, user memberships, OAuth, app scopes or Base routing.
- The companion CRM repository supplies the unapplied `ops/image-cache.service.conf` narrow override. On the read-only preflight, the workspace existed (`claude:cc-action`, `0750`) but the three cache components did not; do not ask the gateway to create them in the protected workspace. During approved maintenance, create those new traversal parents as administrator-owned `root:cc-action` `0750`, and only the final `images` directory as `ccconnect:cc-action` `02750`. Recheck the entire chain immediately before creating anything; refuse symlinks, unexpected existing entries or ownership drift.

## Install and prove

- Install only reviewed gateway code, existing-authority model policy and narrow cache provisioning; no new cleanup service. The process performs seven-day/512-MiB cleanup itself, preserving active leases and unrelated files.
- Test from the actual restarted service mount namespace: gateway can write/sync/rename a synthetic cache image; Claude can read it but cannot write/remove it or create a file beside it; neither identity gains control-plane access. Check at least 1 GiB + four times the raw batch bytes is available.
- Send only synthetic images to a separate Sandbox acceptance configuration; test pure image, mixed text/image, quoted image, four-image limits, resume, rejection/resend, and native-history growth. Original cache and native conversation files are separate storage budgets.
- Slow only synthetic Sandbox approval execution. Owner observes desktop and phone: click acknowledgement → durable accepted card with frozen preview and no buttons → actual result. Repeated clicks must not create writes, extra child cards, or old-state refreshes. Keep the real CRM free of injected failures.
- Run ordinary Feishu document/Base/calendar checks without changing app identity; Quiet, continued session, stale approval truth and one receiver checks. Query receipts read-only, do not stage new plans to learn status.

## Rollback

Stop the candidate receiver before starting the prior compatible binary. Restore only reviewed code/policy/service override; leave verified CRM writes and the current ledger intact. Retain original cache files for their normal finite lifetime or audit them separately; do not delete attachments/history as a rollback shortcut. Verify one receiver and read-only result access again. Report any pending business/unknown receipt explicitly and recover manually, never by resubmitting the write automatically.
