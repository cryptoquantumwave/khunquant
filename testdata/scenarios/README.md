# Writing a Sandbox Lab scenario

A scenario is a YAML file that sets up a locked KhunQuant instance (fake exchanges, fake chat,
scripted model, a clock you control), plays a timeline into it, and says what the lab journal
must show afterwards. Use it only when a bug needs wire, chat, LLM, time or a crash; otherwise
write a cheaper test (unit test, replay case, red-team test).

- Schema: [`schema.v1.json`](schema.v1.json). It is generated from the Go types in
  `pkg/sandbox/scenario` (`go generate ./...`). Point your editor at it for completion.
- The loader is the authority. `sandbox_scenario op=validate` (or
  `./build/khunquant sandbox run --validate-only <file>`) reports every problem with a JSON
  pointer, and checks what the schema cannot (see the end of this file).
- Run one: `sandbox_scenario op=run` with `file`, or `./build/khunquant sandbox run <file>`. A run
  starts its own instance, which counts against the cap (`KQ_SANDBOX_MAX_INSTANCES`: 1 in
  `.mcp.json`, 1 if unset, at most 8), so at the cap stop yours first (`sandbox_instance op=down`).
  Exit status: 0 all pass, 1 an expectation failed, 2 an invalid scenario, 3 an infrastructure failure.
- A file `testdata/scenarios/<area>/<id>.yaml` has `scenario: <id>`. Ids are unique.

## Anatomy

```yaml
version: 1                         # always 1
scenario: my-bug                   # = the file name without .yaml
covers: [H8]                       # review IDs
llm: scripted                      # scripted | replay | live
level: wire                        # wire (default) | event; recorded only in v1
clock: { start: "2026-10-20T09:00:00+07:00", mode: manual }
instance:
  config:                          # merged over the sandbox profile
    proactive:
      enabled: true
      owner: { channel: simchat, chat_id: "dm:owner", sender_id: "simchat:owner" }
      attention: { mode: live }
      perception: { enabled: true }
      triggers: { mode: live }
profile: { timezone: Asia/Bangkok, active_hours: { start: "07:00", end: "22:00" } }
market:
  BTC/USDT: { venue: binance, model: set, price: 98000 }
account:
  binance/main: { balances: { USDT: 10000 } }
setup:                             # runs before the clock moves
  - call_tool: { as: owner, name: set_price_alert, args: { action: create, provider: binance, symbol: BTC/USDT, condition: above, threshold: 100000, recurring: false } }
timeline:
  - at: "+5m"
    market.path: { symbol: BTC/USDT, to: 100500, over: 3m, shape: linear }
  - run_until: "+1h"
expect:
  - id: crossed
    event: { kind: trigger.candidate, trigger: price_cross, subject: BTC/USDT }
```

Other header fields: `status` and `known_bug` (see Known bugs), `runs` and `pass_rate` (repeat
a live scenario), `matrix` (one run per combination; `${name}` is replaced in the file), `base`
(`main` or `stack-head`, informative), `miss` (replay only). A scenario carries no credential and
names no real exchange host; the validator refuses both.

## Time

The clock is manual: nothing moves until a step moves it. `at: "+5m"` is an offset from
`clock.start`; offsets must not decrease. Two ways to move time:

- `advance: 15m` moves by that much.
- `run_until: "+12h"` moves to an offset, an RFC 3339 time or `next 08:00`, in settled chunks.
  `run_until: { until: { message: { to: owner }, count: ">= 1" }, max_sim: 24h }` stops when an
  expectation holds.

Between steps the runner settles the instance (every component idle). A trigger that compares
with an earlier price needs that history, so move the clock far enough first.

## Markets

`market:` seeds a symbol; a timeline step changes it. Steps: `market.set`, `market.path`
(`symbol`, `to`, `over`, `shape`), `market.shock` (`symbol`, `pct`), `market.walk` (`symbol`,
`vol`, `seed`), `market.replay` (`symbol`, `timeframe`), `market.delist`, `market.relist`. The
`venue` of a symbol must match the account that holds it.

Model `recording` plays a recorded price file: `market: { BTC/USDT: { venue: okx, model:
recording, path: testdata/replay/flash_crash/events.jsonl, align: start } }` (the key is the
symbol; see `derivs/flash-crash-recording.yaml`), or the `market.recording` step with `symbol`
and `path` (both required). `path` is relative to the working directory; the runner copies the file into the instance home, and it must be a valid
recording (an empty or invalid file fails the run).

## Chat

Personas are `owner`, `stranger`, `alice`, `bob`; `group:<name>` is a group chat (`team` has alice and
bob); `owner@group:team` speaks in a group.

```yaml
- chat: { as: owner, text: "please install the demo skill" }
- chat.press: { on: { to: "dm:owner", contains: ["Allow KhunQuant to BUY"] }, action: approve }
- chat.react: { on: { to: "dm:owner", contains: ["BTC"] }, emoji: up }
- chat.channel: { state: down }     # the chat channel is down; up brings it back
```

`on` selects the message by `to` and `contains`. Tools run as a persona with
`call_tool: { as: owner, name: ..., args: {...} }`.

## LLM scripts

With `llm: scripted` the model is a list of rules, first match wins:

```yaml
llm_script:
  - id: install
    match: { sender: "simchat:owner", prompt_contains: ["install the demo skill"] }
    respond: { tool: install_skill, args: { slug: demo-skill, confirm: false } }
    times: 1
  - id: any
    match: {}
    respond: { text: "ok" }
```

To prove that a gate holds even if the model is fully compromised, script the model to do the
forbidden thing (see `security/confirm-false-still-needs-approval.yaml`) and expect the gate, not
the model's good behaviour. PR CI runs scripted and replay scenarios only; `llm: live` runs only when someone starts the Sandbox Live workflow by hand (`workflow_dispatch`, with the `SANDBOX_LLM_KEY` secret), or locally with `KQ_SANDBOX_LLM_KEY` and `--llm-budget`.

## Faults and crashes

```yaml
- at: "+1m"
  fault.add:
    match: { venue: okx, path_prefix: /api/v5/market/tickers }
    effect: { error: rate_limit }
    when: { from: "+0m", to: "+4m" }
- restart: { kill_at: { workflow: dn.open, step: place_second_leg, point: after_started } }
```

`fault.clear` removes rules. `restart` takes `graceful`, `kill` or `{ kill_at: { workflow, step,
point } }`, which kills the instance when that step reaches that point and restarts it on the
same home. Examples: `faults/`, `crash/`.

## Expectations

One kind key per entry, plus an optional `id`: `message`, `event`, `decision`, `order`,
`workflow`, `proposal`, `policy`, `probe`, `llm`, `web`, `logs`. The fields of each kind are in the
schema. Modifiers may sit inside the kind's mapping or beside it:

- `count`: `1`, `">= 1"`, `"<= 2"`, `"0"` (absence). Give an absence check a `within` window (advice; the loader does not enforce it).
- `within: { from: "+6m", to: "+10m" }`: offsets from `clock.start`; `during: "22:00-07:00"`: a
  daily window in the profile time zone.
- A value may be negated with `!`: `decision: { reason: "!muted" }`.
- `replay_match: <dedup-key prefix>` (a message expectation only) names a message that must not
  be sent, so an exported replay case (see Debugging) gets a should-not-fire label for that key
  in the `within` window. It needs `count: 0` and a closed `within` (`from` and `to`); the
  validator refuses it otherwise. A positive message expectation with a closed `within` is
  labelled should-fire from the dedup keys of the messages it matched, with no modifier.

Always settle before asserting (the runner does between steps). The runner also adds implicit
checks (`settled`, journal drops). `logs: { }` fails on unexpected error logs.

## Debugging a red run

The report names each failed expectation with what it wanted and what it got. `sandbox run
--keep` keeps the home (logs, config) of a run that did not pass. To look around a live instance,
start one with `sandbox_instance op=up` and use:

- `sandbox_inspect op=journal`, `sandbox_inspect op=decisions`, `sandbox_inspect op=policy`,
  `sandbox_inspect op=workflows` and `sandbox_inspect op=proposals`;
- `list_llm_calls` and `read_llm_call` for what the model saw;
- `read_logs`, or `sandbox_instance op=logs`;
- `sandbox_inspect op=settle` returns `busy` (the components that did not go idle) when a settle fails.

To retry from a known point, take a snapshot (manual clock only): `sandbox_instance op=snapshot`,
list them with `sandbox_instance op=snapshots`, start a new instance from one with
`sandbox_instance op=restore` (refused for live and record instances), and remove one with
`sandbox_instance op=snapshot_delete`. The store keeps at most `KQ_SANDBOX_MAX_SNAPSHOTS` (default
20, at most 200), and a take beyond that is refused until one is deleted.

A green run can become a replay case: `./build/khunquant sandbox run --export-replay <dir> <file>`
(add `--force` to replace a case), or `sandbox_scenario op=run` with `export_replay` (and `force`);
`sandbox_inspect op=export_replay` does it for a running instance. Labels come from the message
expectations.

The launcher can show the journal as a timeline, read-only. Start one launcher per instance with
`KHUNQUANT_HOME=<home> KHUNQUANT_CONFIG=<home>/config.json <launcher> -port <N> -no-browser`. Do not
save settings in that launcher: a save rewrites the instance's config.

## Several instances

With `KQ_SANDBOX_MAX_INSTANCES` above 1, `instance` (the name) is required on every tool call
while more than one instance runs; the error lists them. It is optional while exactly one runs.
`sandbox_instance op=list` shows them, and `down`, `restart` and `logs` take `name`.

## Known bugs

A scenario for a bug that is still open has `status: known_bug` and `known_bug: <review ID>`.
It is skipped (`known_bug <ID>` in the report) unless `KQ_SCENARIO_KNOWN_BUGS=1` is set, or
`known_bugs: true` is passed to `sandbox_scenario op=run`. The PR that fixes the bug sets
`status: active` (or removes it) and removes `known_bug`, in the same change.

## Worked examples

- [`attention/alert-survives-channel-outage.yaml`](attention/alert-survives-channel-outage.yaml) (H8): a channel outage, `run_until`, absence windows.
- [`orders/mute-on-warning-keeps-protective-critical.yaml`](orders/mute-on-warning-keeps-protective-critical.yaml) (S116A-1).
- [`security/group-owner-refused.yaml`](security/group-owner-refused.yaml) (H7, a known bug).
- [`crash/phase3-dn-open-before-send.yaml`](crash/phase3-dn-open-before-send.yaml): a crash point.

## What only the loader checks

The schema checks keys, closed value sets and required arguments. These rules live only in
`pkg/sandbox/scenario` (`Validate`), so a green schema is not a valid scenario: grammar and
ranges of `count`, `at`, `within`, `during`, durations and `pass_rate`; `at` order and no `at` in
`setup`; exactly one kind key per step and expectation; unknown fields inside `instance.config`;
order values (unified symbol, status, side); event payload paths; `known_bug` only with
`status: known_bug`; unique ids; the ban on credentials and real exchange hosts; the ban on `as: cli` in a `call_tool` step; and what the
running instance lacks.
