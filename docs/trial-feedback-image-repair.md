# Trial input and approval feedback repair

Spec source: the Owner's approved “图片传递、跟进草稿接续与审批反馈” implementation request, 2026-09-06. Fixed points: gateway `492a0e898b825ee60b24b3ca8cb6f07014d940ef`, CRM `7cbc4e7e7b57d1e5b8fbb8e3fb1b2f61ad688417`. Existing approval-status policy/eval overlay is preserved, not a new command-parser change.

## Required behavior

- Keep one authenticated Owner, each write individually approved, and two independent customer/follow-up approvals. Do not change commands, CRM fields, permissions, identity, or introduce automatic business recovery.
- Click acknowledgement is not durable approval. Callback responds within three seconds. After the ledger claim succeeds, the original card shows persistent accepted/executing text in the title and original button area, with the complete frozen read-only preview and no approve/modify/cancel buttons or fake percentage.
- Only hosted approval cards opt into `Card.SharedUpdate` / Feishu `update_multi`. Placeholder, activation, intermediate and terminal cards all opt in; ordinary cards retain default behavior.
- Preserve host CAS/exactly-once execution. `replayed` callbacks are toast-only; do not reconstruct business facts from callback fields. Claim transport/ledger failures cannot overwrite newer cards. Lost execution receipts are unknown, never proof of no write.
- Refresh outside the callback wait. If intermediate refresh is unconfirmed, do not send a potentially competing terminal PATCH. Complete the business operation once and send one logical exceptional notice, with a stable message UUID. Otherwise retry only the same terminal card at most three times. Never retry a CRM write or add a recovery queue/polling framework. Diagnostics contain phase, elapsed time and result category, not customer facts or API errors/identifiers.
- Deliver pure image, mixed text/image, multiple images, quoted images and queued turns completely and in order. A failed download/validation/cache write rejects the whole batch with a resend instruction; never silently send remaining text/images. Do not let a later text advance the message watermark ahead of an in-flight image download.
- Fixed limits: four images, 5 MiB each, 10 MiB combined. Bound network reads before the SDK buffers the body and share the budget across quoted/forwarded/current images. JPEG/PNG/GIF/WebP only; animations first frame only, no resize. Reject malformed/unsupported inputs and native-incompatible dimensions explicitly.
- Dedicated original-image cache: seven days or 512 MiB, whichever is reached first; evict oldest inactive entries. Count temporary files and concurrent reservations; lease active images, including same-session rereads, until result/process exit. Cleanup at startup, new requests, end of turn and hourly. Only owned filename entries under the dedicated root, no symlinks/path escape/other attachments or CRM data. Pause new images when disk free is below 1 GiB plus four times raw batch bytes.
- Gateway alone writes the cache, Claude's group only reads. Preserve service umask, workspace/control-plane permissions. Cache capacity does not bound native Claude history; measure native history growth separately. If an original expires and must be reread, ask for resend.
- One authoritative model-rule section preserves the confirmed follow-up draft through owner clarification, owner retry, “other data unchanged” and “follow-up separately approved”. Only explicit current-request withdrawal removes it; never restore an earlier cancelled operation. Reuse existing `followup` field, omit unspecified optional fields, collect required facts together; no new tool or draft database. Images are data, never authorization or approval instructions.

## Release gates

1. Failure-first regressions for image-save loss, draft omission and slow/failed card feedback; repeated/concurrent callbacks, late PATCH, unknown receipt and notice response loss tests.
2. Complete image bytes/order, separate Unix user read-only access, continuation, expiry/capacity/concurrency/low disk and malformed/partial downloads. Native accepted-limit boundary check, including native history growth.
3. Synthetic paired model cases, three trials each, saved evidence and independent semantic review: normal/poisoned images, retained/withdrawn draft, owner retry, create+follow-up/create-only. Prior approval-truth cases must remain intact.
4. Existing Go/Python regression, CUJ, build, vet and race checks; ordinary Feishu, Quiet, session continuation and single receiver remain release gates.
5. Sandbox-only slow approval observation on desktop and phone, persistent button-area status and actual final receipt. Never inject faults in the real CRM. Mock tests are not UI acceptance.
6. Standards/spec `code-review`, `ponytail-review`, independent engineer review, then merge reviewed code. Production installation still requires separate Owner-approved maintenance time.
7. Roll back code/rules only, preserving real records and ledgers. Record resend, duplicate-click and manual-intervention counts during resumed trial; passing tests is not proof of usability.

## Evidence status

Implementation, automated verification and independent code/model reviews are complete; see `trial-feedback-validation.md` for evidence and limitations. No production deployment or live CRM write is part of this branch. Test-only files and synthetic transcripts must never include customer screenshots, credentials or real contact details. Sandbox desktop/phone and live service checks remain explicit maintenance-window gates; source merge does not authorize installation.
