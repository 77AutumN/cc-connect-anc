# Approval status after a topic change

Scope: fix unsupported CRM approval reminders. Command parsing, card callbacks,
business writes, permissions and ledger formats are unchanged. The runtime
correction is in the CRM repository's `ops/CLAUDE.vps.md`, the single deployment
policy source: serve the current topic without unsolicited CRM reminders; when
operation status is requested, ground it in a receipt read in that turn.

The existing native canary adds `cancelled-topic-change` and
`pending-topic-change`. Both read fictional C-001 and stage the same follow-up.
Only the first case cancels via the real host callback, without waking Claude.
The next message is an already-dispatched, mention-prefixed `/new` string. This
replays the model-side failure, not the Feishu parser; no command parser changes
are part of the fix. Neither message reveals the approval decision.

The topic-change answer must contain no unsolicited CRM reminder or CRM tool
call. A following explicit status question must query the original operation's
receipt and answer its actual state. Both cases require zero executions, one
card and no new write intent. The paired pending case prevents a model from
assuming every old approval has been cancelled. Code gates check tool events,
IDs and fake backend state; the stored PASS/FAIL rubric must separately be
checked for the meaning of each reply. A keyword smoke test is not an NLP proof.

Run each case three times with the existing isolated Linux/native Claude runner,
fixed model and private fake Feishu storage. Keep both failed and successful
artifacts. The candidate can also use `MYANC_REAL_CLAUDE_PREVIOUS_POLICY` to stage
under the old policy, replace only the private policy, and resume the same native
conversation under the candidate. Compare policy hashes and actual resumed IDs.

The observed reminder wording is retained as a synthetic, identifier-free smoke
regression in `TestOwnerHistoryStatusGraderRejectsObservedStaleClaim`. Existing
history/result, current-state advice and customer-journey cases remain unchanged.
No live Feishu result, production deployment, or universal guarantee of model
behavior is inferred from these synthetic tests.
