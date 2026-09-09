# Team Brain integration (disabled until separately approved)

This PR adds a second hosted action domain alongside the existing CRM adapter.
Three model commands (`knowledge_search`, `knowledge_read`, `knowledge_propose`)
call the private Python subprocess. Only authenticated card callbacks can claim
and execute a persisted proposal. The gateway forwards its authenticated
platform/user/chat/session/project tuple and exact published card ID.

Code and operational contract: private repository `77AutumN/myanc-team-brain`,
`docs/spec.md`, `docs/tools.md`, `docs/release-checklist.md`.

Opt in using `CC_TEAM_BRAIN_COMMAND` (protected absolute executable wrapper) and
`CC_TEAM_BRAIN_PROJECTS` (exactly six fixed existing project names). Configuration
is consumed and removed before agent creation. Each project must run its agent
under a separate unprivileged account and use a fixed workspace. The Python host
independently checks all six exact identity tuples and the fixed Wiki root.
The model receives only `TEAM_BRAIN_SESSION_TOKEN`, never host configuration.

The shared loopback listener chooses exactly one live engine by token; ambiguous,
stopped or rebound sessions fail closed. Existing CRM tools and cards retain their
adapter. Knowledge preview publication binds its own exact platform message ID.
No separate listener, public endpoint, permission changes or deployment is added.

Baseline warning: this integration starts from GitHub main
`37299c18bc07585222cf738a2f68ef9f77da96fc`. The deployed September 9 gateway has
additional six-environment/CRM navigation work not on that base. Do not deploy a
binary from this older base over production. Reconcile that work in its owning
maintenance task, then rebase this patch and repeat the combined regression gate.
Do not copy unrelated local modifications into this PR.

Tests include combined host tool/card routing, six-engine token isolation,
unchanged existing-tool behavior, exact card binding, subprocess principal
forwarding and secret exclusion, five-language cards, and a real Engine user
journey through ReceiveMessage. Python lifecycle tests live in the independent
repo. CI runs checks only; production scopes, configuration and restart remain
separately approved release actions.
