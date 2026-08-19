# Linear as an optional work source (step 01)

**Status:** design / not implemented
**Scope:** replace step 01 ("Issue Filed") of the Hive loop with Linear, optionally, without changing steps 03–07.

## Context

The [How it works](https://hive.kubestellar.io/#how-it-works) loop starts at step 01, "Issue
Filed" — a bug report, feature request, or dependency alert lands on the repo. Today that is
always a GitHub issue. Teams that plan in Linear and host code on GitHub have no way to point a
hive at their real backlog.

This document plans a `work_source` abstraction so a hive can enumerate its work list from
Linear while GitHub remains the code host for branches, PRs, CI, and merges.

## Goal

```
step 01  Issue filed      →  Linear issue  (was: GitHub issue)
step 02  Triage           →  labels/assignee written back to Linear
step 03  Fix agent        →  unchanged (GitHub worktree + PR)
step 04  Coverage gate    →  unchanged
step 05  Review           →  unchanged
step 06  Merge            →  unchanged
step 07  Rerun            →  unchanged
```

## Non-goals

- Replacing GitHub as the code host. PRs, CI, and merges stay on GitHub.
- Supporting Jira, Asana, or Shortcut in this pass. The seam should not *preclude* them, but
  only Linear gets an implementation.
- Bidirectional sync between Linear and GitHub issues. A hive uses one work source, not two.

---

## Why this is tractable

The enumeration entry point is a single seam, and everything downstream of it is already
source-agnostic.

| Component | File | Coupling to GitHub |
|---|---|---|
| Enumeration | `pkg/github/client.go:287` `EnumerateActionable` | **total** — this is the seam |
| Only caller | `cmd/hive/main.go:4104` `runEvalCycle` | one call site |
| Governor | `pkg/governor/governor.go:250` `Evaluate` | **none** — takes four `int`s |
| Classifier | `pkg/classify/classifier.go:84` `Classify` | **none** — reads `Title` + `Labels` strings |
| Scheduler | `pkg/scheduler/scheduler.go:477` | formats issues into kick text |
| Dashboard | `pkg/dashboard/status_builder.go:957` | ships `[]any`; frontend links `i.url` verbatim |

`ActionableResult` is referenced in only seven non-test files (`pkg/scheduler/scheduler.go` carries 16 of the
references; the rest are one to five each, including `cmd/hive/celwire.go`). `github.Issue` appears in only two
packages outside `pkg/github`.

---

## Design

### The seam

Introduce a `WorkSource` interface and make `runEvalCycle` depend on it instead of `*github.Client`:

```go
// pkg/worksource/worksource.go
type Source interface {
    // Enumerate returns the current actionable work list.
    Enumerate(ctx context.Context) (*github.ActionableResult, error)
    // Kind identifies the backing service ("github", "linear") for logging,
    // dashboard badges, and prompt-template selection.
    Kind() string
}
```

`*github.Client` satisfies this already. `pkg/linear.Client` becomes the second implementation.

`ActionableResult` stays the wire type on purpose — moving it out of `pkg/github` would touch
the dashboard, hub heartbeat, and persisted `/data/last-actionable.json` schema for no
functional gain. Rename later if a third source arrives.

### Identity mapping

`github.Issue.Number` is an `int`; Linear identifiers are `ENG-123`. Add a `Key` field rather
than overloading `Number`:

```go
type Issue struct {
    Repo   string // GitHub: "org/repo".  Linear: mapped repo (see config)
    Number int    // GitHub: issue number. Linear: numeric part of the identifier
    Key    string // "" for GitHub. "ENG-123" for Linear.
    // ...unchanged
}
```

Render `Key` when non-empty, `#Number` otherwise. Consumers to update: kick messages
(`scheduler.go:477`), dashboard pills (`pkg/dashboard/static/index.html:6745`), SLA
notifications (`main.go:4231`), `writeMergeEligible` (`main.go:4977`), the duplicate-PR claim
ledger (`pkg/github/prclaims.go`), and the contributor relay's URL builder
(`pkg/dashboard/contribute_triage.go:246`, which currently hardcodes a github.com URL instead of
carrying the source's own).

#### `%s#%d` is an identity key, not just a display string

This is the finding that most changes the Phase 1 estimate. `fmt.Sprintf("%s#%d", ...)` appears
at 44 sites in 10 non-test files, but they are not one population. Classified:

| Role | Sites | Where |
|---|---|---|
| **Identity key** — task admission, hold state, claim ledger, relay active-task set | **30** | `pkg/dashboard/contribute_ws.go` (18), `contribute_sse.go` (6), `contribute_opportunistic.go` (4), `contribute_prlink.go` (1), `pkg/github/prclaims.go` (1) |
| Display / message text | 12 | `pkg/scheduler/scheduler.go` (7, all kick-text `WriteString`), `pkg/github/advisory.go` (4, all inside `fmt.Errorf`), `cmd/hive/main.go` (1, SLA notification) |
| Out of scope | 2 | `pkg/knowledge/{bead_synthesizer,curator}.go` — both use `owner/repo#number` and one refers to a PR, not an issue |

The 30 identity-key sites are the ones that matter. `contribute_sse.go:219` documents the format
as load-bearing, and `contribute_prlink.go:92` already wraps it as `prLinkKey` — but
package-private and used only inside `pkg/dashboard`.

Two consequences:

1. A "render `Key` when non-empty" change is **not** sufficient. Two Linear teams can both have
   issue 42; `repo#42` would collide and two distinct tasks would share one admission key.
2. Phase 1 should promote one shared helper and convert those 30 sites to it. The conversion is
   mechanical and behaviour-preserving for GitHub — the helper returns exactly today's string
   when `Key` is empty — so it can land as a standalone PR ahead of any Linear code.

**Put the helper on `github.Issue`, not in `pkg/worksource`.** Landing it in a new package would
create the very package this document is asking to have blessed, and would drag the 90% coverage
floor onto an otherwise-empty package. As a method on the existing `Issue` type it is pure
de-duplication inside `pkg/github` and `pkg/dashboard`, which already depend on each other
(`pkg/github` imports only `beads`, `advisory`, `resolve`, and `config`, so there is no cycle).
If `pkg/worksource` is later approved, the helper moves or is aliased in one line.

### Team → repo mapping

The dashboard groups all work by repo (`status_builder.go:920-961`) and agents need to know
which repo to clone. Linear has teams and projects, not repos, so the mapping is explicit config
rather than inferred.

### Field mapping

| `github.Issue` | Linear GraphQL |
|---|---|
| `Title` | `issue.title` |
| `Labels` | `issue.labels.nodes[].name` |
| `Assignees` | `issue.assignee.name` (single) |
| `Author` | `issue.creator.name` |
| `CreatedAt` | `issue.createdAt` |
| `URL` | `issue.url` |
| `Number` | numeric part of `issue.identifier` |
| `Key` | `issue.identifier` |
| `Repo` | from `linear.teams[].repo` mapping |

Hold semantics (`HoldLabels` in `client.go:204`) map to Linear labels of the same name, or to a
configured set of workflow states — decide during Phase 1 (see Open questions).

---

## Config sketch

```yaml
project:
  org: your-org
  repos: [repo-one, repo-two]
  primary_repo: repo-one

work_source:
  type: linear                # github (default) | linear
  linear:
    api_key: ${LINEAR_API_KEY}
    teams:
      - key: ENG
        repo: repo-one        # which repo agents clone for this team's work
        states: [Todo, Backlog]   # workflow states considered actionable
    hold_labels: [hold, blocked]
```

Absent a `work_source` block, a hive behaves exactly as it does today. That is the
compatibility contract for every existing deployment.

---

## Phases

### Phase 1 — read path

Ships a hive that enumerates, classifies, kicks, and displays Linear work. Agents still do all
their writing on GitHub.

- [ ] `pkg/worksource` — the `Source` interface; `*github.Client` conforms unchanged
- [ ] `pkg/linear` — GraphQL client, issue enumeration with pagination, `Enumerate` mapper
- [ ] `Issue.Key` field + render sites listed above
- [ ] `work_source` config block + validation in `pkg/config`
- [ ] Wire the seam at `cmd/hive/main.go:4104`
- [ ] Dashboard source badge so an operator can see where work is coming from

**Exit criteria:** point a hive at a Linear team; the governor changes mode from Linear queue
depth, kicks render Linear identifiers, and dashboard pills deep-link to Linear.

### Phase 2 — write path

This is the bulk of the work and the only part with new security surface.

- [ ] Attach the Linear MCP server to agents via the existing `connections:` mechanism
      (`config.ConnectionConfig`, `config.go:359`). `connectionMCPFlags`
      (`pkg/agent/manager.go:6170`) is a 12-line switch that today only handles the `claude`
      backend — extend per backend.
- [ ] **Proxy enforcement for `api.linear.app`.** Today `NeedsMITM` (`pkg/proxy/rules.go:59`)
      returns true only for `api.github.com`; every other host falls through to `tunnelDirect`
      (`pkg/proxy/github_proxy.go:793`). Linear traffic therefore egresses **completely
      ungated**. Linear is a single `POST /graphql` endpoint, so the existing path-regex
      `ProxyRule` model does not apply — ACMM enforcement needs GraphQL operation-name and
      mutation-body inspection. **This is new enforcement machinery, not a new rule.**
- [ ] Map ACMM modes onto Linear mutations (e.g. `issueUpdate` label/assignee ⇒ `ISSUES_ONLY`),
      and update `docs/acmm-policy-matrix.md`
- [ ] Linear-native triage instructions in the scanner kick template
- [ ] Linear webhook channel. `pkg/channels/webhook.go` is hardcoded to GitHub's HMAC scheme and
      `X-GitHub-Event` header; Linear needs a sibling handler.

**Exit criteria:** a scanner agent labels and assigns a Linear issue, and an agent attempting a
mutation above its ACMM level is refused by the proxy with a test proving it.

### Phase 3 — linkage and polish

- [ ] `ENG-123` claim parser beside the `Fixes #N` parser in `pkg/github/prclaims.go`. This guard
      exists because a restart storm once filed nine near-identical PRs against one issue (see
      the file header); it must not regress when work comes from Linear.
- [ ] Claim ledger keyed on `Key` when present
- [ ] Hub heartbeat + leaderboard fields (`pkg/hub/server.go:155` `ActionableIssues`)
- [ ] `hive.yaml.example` entry, `docs/architecture.md` §1 and §3 updates, operator runbook

---

## Testing

The repo enforces a **90% per-package coverage floor**, checked hourly with auto-filed issues
(`.github/workflows/coverage-hourly.yml`). `pkg/github` runs roughly 40 test files to 18 source
files. Budget test code at or above 1:1 with production code for `pkg/linear` and `pkg/worksource`.

Two gate details that constrain how these packages land. Only `dashboard` (78), `hub` (89), and
`agent` (89) carry documented threshold overrides, and the workflow comment forbids adding
entries without justification — so **new packages get no override and must land at 90%**.
Separately, a package reporting `? [no test files]` is a hard gate failure in its own right, not
a skip. Neither `pkg/linear` nor `pkg/worksource` can be merged as a skeleton ahead of its tests.

Specific cases worth naming now:

- Enumeration failure must **not** report an empty queue. `EnumerateActionable` returns an error
  when every repo fails precisely so the governor does not idle the fleet (`client.go:332`).
  `pkg/linear` must preserve that behaviour on API failure or rate limit.
- Pagination beyond one page of Linear results.
- An issue whose Linear team has no `repo` mapping — skip with a warning, never crash the cycle.
- Proxy: a Linear mutation below the agent's ACMM level is refused (Phase 2).

---

## Risks

| Risk | Mitigation |
|---|---|
| GraphQL ACMM enforcement is new machinery with real security surface | Phase 2 is separable; Phase 1 ships value with agents read-only against Linear |
| Linear rate limits differ from GitHub's; the eval loop runs every `eval_interval_s` | Reuse the fail-closed pattern at `client.go:332`; cache last good result |
| `Repo`-keyed assumptions are spread wider than the six `ActionableResult` files | Phase 1 adds `Key` without removing `Number`, so unconverted call sites keep working |
| Two sources of truth if a team runs both | `work_source.type` is exclusive by construction — no dual-source mode |

## Open questions

1. **Hold semantics** — Linear labels, workflow states, or both? Leaning workflow states, since
   that is how Linear teams actually express "not ready".
2. **Multi-team hives** — is one hive to many Linear teams a real requirement, or is team ⇔ hive
   sufficient for v1?
3. **Issue closing** — rely on Linear's native GitHub integration (branch-name linking, auto-close
   on merge), or close explicitly via the API in step 06? Native integration is free and is the
   recommended default; explicit close is a Phase 2 stretch.
4. **Fix-agent context** — does the agent need the Linear issue's full comment thread in its
   worktree prompt, or is title + labels + description enough, as it is for GitHub today?

## Recommendation

Ship Phase 1 standalone. Linear becomes the work source, agents still open PRs on GitHub, and
Linear's native GitHub integration closes the issue on merge. Phase 2 then becomes an optional
upgrade — "agents can also triage *in* Linear" — rather than a blocker for anyone who just wants
their hive pointed at the backlog they actually use.
