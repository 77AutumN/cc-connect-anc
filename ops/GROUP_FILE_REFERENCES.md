# Explicit group file continuation (candidate, off by default)

For an already provisioned fixed Feishu route using native file work, opt in
with `group_references = true` under `[projects.file_work]`. Both actors must
use the same Bot, actual group, protected file ledger and snapshot directory.
Account/session isolation, existing business approvals and pinned recipients
are unchanged. Follow the CRM `ops/RELEASE.md` before enabling any route.

Reply directly to a delivered Word/Excel file. The host copies that accepted,
hash-checked snapshot into the replying actor's read-only inputs, preserving its
source work, delivery, version and receipt. The actor's own Claude makes the
revision; the existing FileSender returns it to that actor's original group
route. Repeating the same reference continues the imported work. Source files,
old versions and private conversations remain with their original actors.

This is artifact handoff, not a shared Claude session or a global version
counter. Other actors' task/text replies can be ambiguous; reference the exact
file instead. Unrelated group chatter is not ingested. Private/cross-group or
other-Bot references, unknown delivery results and corrupt snapshots are refused.
No new endpoint, database, queue, dependencies or runtime identity is introduced.

The optional paired CRM project-context policy adds one narrow exception:
accepted project-group snapshots may enter a registered member's private work
after host checks for membership, original group and same-project binding.
Queued imports repeat that check before copying. No private-to-group send is added.

The first group work marks the existing ledger as version 2 so older binaries
cannot silently erase source metadata. Private-only ledgers retain version 1.
After that mark, rollback disables group routes while keeping a version-2-aware
gateway; if a binary rollback is necessary, pause file processing until a compatible
build is available. Never downgrade the ledger or restore an older business snapshot.

The first authorized cross-chat private import marks version 3. After this,
a version-2-only gateway cannot open the shared file ledger even when the new
project flag is off. Disable project entry points but keep a version-3-aware
gateway; if replacing that binary is unavoidable, pause file processing until
a compatible build is available. Keep all project history, receipts and files.
