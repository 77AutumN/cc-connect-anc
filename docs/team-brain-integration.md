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

Baseline: the owning maintenance task merged deployed six-environment routing and
CRM navigation in PR #4, main `7895bac`. This patch was replayed on that base,
preserving CRM `open`, all six hosts, strict interaction routes, ConversationOnly
and the single shared tool listener. Only knowledge changes are in this PR.
Before activation, still compare the actual deployed binary and strategy hashes
against the selected release manifest. No local maintenance changes are included.

Tests include combined host tool/card routing, six-engine token isolation,
unchanged existing-tool behavior, exact card binding, subprocess principal
forwarding and secret exclusion, five-language cards, and a real Engine user
journey through ReceiveMessage. Python lifecycle tests live in the independent
repo. CI runs checks only; production scopes, configuration and restart remain
separately approved release actions.
