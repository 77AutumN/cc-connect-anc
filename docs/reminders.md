# One-shot reminders: implementation and acceptance contract

Source: user-approved team Bot reminder plan, 2026-09-10. Deployment remains
separately authorized. This document records requirements, not test results.

- Only the existing owner, partner and test identities, derived from six verified
  fixed private/group environments. No new receivers or tool listeners.
- Explicit natural-language personal create/update/cancel needs no extra card.
  Ambiguous intent/time asks; quoted documents/messages do not authorize actions.
  Beijing timezone, date-only 09:00, full date/weekday/time/content/ID receipt;
  reject past dates without silently rolling forward. No recurring/assigned tasks.
- Private tools see all own reminders. Group tools see/manage only own reminders
  originating in that group. A group full-list request is host-delivered privately;
  never return private bodies to the group model.
- Trusted host sender-to-private mapping only. Tool fields cannot select actor,
  destination, project or path. No CRM read/write at creation or delivery; no model
  or shell execution when due. `/new` and `/stop` leave reminders intact.
- Separate protected SQLite database. Explicit initialization only; missing,
  corrupt or unwritable state fails closed for reminders, without resetting the
  database or affecting CRM/knowledge. Persist before success. Original message
  identity plus request deduplicates retries, but new explicit messages stay new.
- Atomic version checks, cancel and dispatch claims. In-flight cancellation cannot
  promise recall, but prevents retries. Restart catches up overdue reminders,
  batching by person and paging without omission. Freeze text/UUID before sending.
- Feishu UUID dedup window is one hour. Retry unchanged within window. Beyond it,
  prioritize delivery, mark possible duplicates. Backoff 1/5/15/30 minutes then
  hourly. Permission denial, revocation or changed route pauses, never reroutes.
  Persist acceptance receipt; accepted does not mean read. No bodies/DM IDs/tokens
  in ordinary logs; no urgent/SMS/phone privileges.
- Validate three-domain tool/card traversal and duplicate domain/command refusal.
  Test time/CRUD/idempotency, three-person isolation, concurrency, crash/timeout/
  missing receipts/window crossover, corrupted/unwritable DB, route revocation,
  CRM/knowledge/six-environment regressions, image/new/stop, real Engine multi-step
  CUJ. Natural-language positive and negative cases each require three runs and
  independent review; deterministic mocks alone do not prove model behavior.
- Two focused PRs, independent Spec/Standards review, full exact-SHA CI and merge,
  dual-SHA candidate build with explicit cross-repo tests. Default off. No VPS
  changes until maintenance authorization, backup and explicit DB initialization.
  Preserve knowledge live configuration; never replace all runtime files from an
  older template. Owner/test live acceptance later; partner untested is not passed.
  Rollback stops scheduler first and preserves database, receipts and business data.

Feishu dedup semantics: [official SDK](https://larksuite.github.io/oapi-sdk-java/com/lark/oapi/service/im/v1/model/CreateMessageReqBody.Builder.html).
