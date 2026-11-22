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

A `chat` step whose message does not reach the instance (a group message with no `mention: true`, a
persona that `allow_from` refuses) fails the step, so a scenario of "must not happen" checks cannot pass
without a turn. A scenario that tests the refusal says so: `chat: { as: stranger, text: hi, expect_undelivered: true }`
(and then a delivered message fails). An argument the chat ops do not know (`mentoin: true`) is refused.

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
the model's good behaviour. PR CI runs scripted and replay scenarios only; `llm: live` runs only when someone starts the Sandbox Live workflow by hand (`workflow_dispatch`, with the `SANDBOX_LLM_KEY` secret), or locally: see "Live runs" below.

## Live runs

`llm: live` talks to a real model. `sandbox run` selects files with `--llm`, which defaults to
`offline` (scripted and replay), so a live file named on its own is validated, skipped, and the run
exits 0 having started nothing (it prints a hint). A live run needs all of: `--llm live` (or `all`),
`--llm-model <protocol>/<model>`, `--llm-budget`, and `KQ_SANDBOX_LLM_MODELS` in the environment of
whoever runs it, listing that exact `<protocol>/<model>` (the protocol is part of the entry). A file with
`status: known_bug` also needs `KQ_SCENARIO_KNOWN_BUGS=1`. Run one row, one run first: `--runs 1`
caps every matrix row, so a file with 2 rows runs 2 instances. Give `--timeout` hours for a slow model
(it caps all the runs of one scenario; the default is 5 minutes).

**A hosted model, with a key** (`anthropic-messages`, `openai`, `openrouter`; the key is read from the
environment, never from a flag, and goes only to that protocol's host):

```bash
KQ_SANDBOX_LLM_KEY=sk-... KQ_SANDBOX_LLM_MODELS=anthropic-messages/claude-sonnet-4.6 \
  ./build/khunquant sandbox run --llm live --llm-model anthropic-messages/claude-sonnet-4.6 \
  --llm-budget 5USD --runs 1 --timeout 30m testdata/scenarios/llm/live-smoke-portfolio-question.yaml
```

A USD budget needs a price for the model (`claude-sonnet-4.6` is built in; add another with
`KQ_SANDBOX_LLM_PRICES=model=in:out`, USD per million tokens). A token budget (`200000tok`) needs none.

**A local OpenAI-compatible server** (an MLX or llama.cpp server on this machine). It needs no key and
no price, and the base must be loopback (`localhost`, `127.0.0.1` or `[::1]`; anything else is refused):

```bash
KQ_SANDBOX_LLM_MODELS='openai/<id>' \
  ./build/khunquant sandbox run --llm live --llm-model 'openai/<id>' \
  --llm-api-base http://127.0.0.1:8080/v1 --llm-budget 20000000tok --runs 1 --timeout 2h \
  testdata/scenarios/llm/live-smoke-portfolio-question.yaml
```

`<id>` is the `id` field of `curl http://127.0.0.1:8080/v1/models`, written as the server reports it. A
model whose id is an absolute path gives two slashes after the protocol (`openai//abs/path/to/model`,
in `--llm-model` and in `KQ_SANDBOX_LLM_MODELS` alike). The instance is sent a fixed placeholder key,
never one from your environment, may reach only that loopback host, and counts the budget in tokens
(`--llm-budget` stays required: it is the run-away guard). The same options exist on `sandbox up`
(`--llm-api-base`, `--llm-call-timeout`) and on the `sandbox_instance` and `sandbox_scenario` MCP tools
(`llm_model`, `llm_budget`, `llm_api_base`, `llm_call_timeout`, with `sandbox_scenario op=run`).

**A slow model.** A local 26B model takes 10 to 60 seconds a call. The lab waits for a model call in
flight before it moves the manual clock or reads an expectation: the settle's wall cap does not run
while a call is in flight (the clock stays put, so a `within` window is judged in simulated time, not
wall time), and a `run_until` has no wall ceiling on a live run beyond `--timeout`. One call is cut at
`--llm-call-timeout` (a Go duration, default `180s`); a run with a call that timed out, or that ran
out of wall time, is infrastructure (exit 3), never a failed expectation. Expect a simulated day at a
30-minute heartbeat to take a long while: `legacy-heartbeat-no-repeat` is about 45 calls a run.

**Memory.** A 26B model holds a lot of RAM, and the lab refuses to start an instance with less than
`KQ_SANDBOX_MIN_FREE_MB` (default 1024) free; do not lower it to fit. Run live-model scenarios with the
owner's own gateway stopped, and one instance at a time (`KQ_SANDBOX_MAX_INSTANCES=1`, one `sandbox run`
or `go test -p 1`). If a run says "not enough free memory", stop something and run that file alone.

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

- `count`: `1`, `">= 1"`, `"<= 2"`, `"0"` (absence). Give an absence check a `within` window (advice; the loader does not enforce it, but see the window check below).
- `within: { from: "+6m", to: "+10m" }`: offsets from `clock.start`; `during: "22:00-07:00"`: a
  daily window in the profile time zone, else in the offset `clock.start` is written in (the loader
  warns on stderr when a file uses `during` or `run_until: next HH:MM` and sets no `profile.timezone`).
- An absence check (`count: 0`, `"<= 2"`) with a `within` fails when the run's sim clock stopped before the
  window ended (or began): the report names both times. "+26h" on a run that ends at +1h proves nothing.
- A "must not happen" check must not pass because of a typo, so the loader checks the values:
  `to` (a persona, `dm:<persona>`, `group:<name>` or a glob), `severity`, `trigger` and `workflow.kind`
  must exist (the sets come from the code), and a `count: 0` on a payload path below a part that declares no
  keys (`args.*` of `tool.call`, `event.payload.*`) is refused unless the same file reads that path in a
  positive expectation, which fails when the key is misspelt.
- `within` and `during` apply to journal kinds and to `workflow` (the time it was created);
  `probe` and `logs` have no timestamps, and the validator refuses a window on them.
- `order: { venue: okx }` counts the orders the simulator's ledger accepted (`exchange.order`,
  `create`), whatever path the adapter used (ccxt creates OKX orders through `batch-orders`).
  Bitkub has no ledger: `order: { venue: bitkub }` and an `order` with no venue read the request log.
- With a `matrix`, every row must meet `pass_rate` by itself; a row that meets it with failed runs
  prints `pass (flaky, 8 of 10)`, and a row below it fails the scenario.
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

`sandbox_instance op=up` takes a scenario's `instance:` block, so a hand-driven instance can be set up like a run's:
`config_patch` (the `instance.config` merge patch: give it an owner and proactive with
`{"proactive":{"enabled":true,"owner":{"channel":"simchat","chat_id":"dm:owner","sender_id":"simchat:owner"}}}`),
`workspace` (`instance.workspace`: relative path to file content) and `accounts` (`instance.accounts`:
venues that get the placeholder exchange account; seed its balances with `sandbox_account`). They
pass the checks a scenario's do (a credential, a lock-owned key, a URL off this machine, a workspace
path that leaves the workspace and a venue with no simulator are refused with the runner's error).
The clock is manual unless you pass `clock_mode: realtime` or `scaled` (the CLI `sandbox up`
defaults to realtime; pass `--clock-mode manual`). `sandbox_clock advance`, `sandbox_run_until` and
`sandbox_instance op=snapshot` need a manual clock. Then:

- `sandbox_inspect op=journal`, `sandbox_inspect op=decisions`, `sandbox_inspect op=policy`,
  `sandbox_inspect op=workflows` and `sandbox_inspect op=proposals`;
- `list_llm_calls` and `read_llm_call` for what the model saw;
- `read_logs`, or `sandbox_instance op=logs`;
- `sandbox_inspect op=settle` returns `busy` (the components that did not go idle) when a settle fails.

To retry from a known point, take a snapshot (manual clock only): `sandbox_instance op=snapshot`,
list them with `sandbox_instance op=snapshots`, start an instance from one with
`sandbox_instance op=restore` (refused for live and record instances), and remove one with
`sandbox_instance op=snapshot_delete`. A restore with no `name`, or with the name of the instance
the snapshot was taken from, replaces that instance if it is running; give another `name` to start
it beside the first. A snapshot is restored only by the build (version and commit) that took it.
The store keeps at most `KQ_SANDBOX_MAX_SNAPSHOTS` (default 20, at most 200), and a take beyond
that is refused until one is deleted.

A green run can become a replay case: `./build/khunquant sandbox run --export-replay <dir> <file>`
(add `--force` to replace a case), or `sandbox_scenario op=run` with `export_replay` (and `force`).
Labels come from the scenario's message expectations. Through MCP the directory must lie inside
the server's working directory, and `force` replaces only a directory that is a replay case:
anything else there is refused. When the case does not reproduce every label, the run says so
(`export warning:` on the CLI, `export_warnings` in the MCP result) and still writes it.
`sandbox_inspect op=export_replay` (with `case` and `labels`) is the step underneath: it writes
the case inside the instance home (`lab/export/<case>/`), which `down` deletes, and copies
nothing out.

The launcher can show the journal as a timeline, read-only. Start one launcher per instance with
`KHUNQUANT_HOME=<home> KHUNQUANT_CONFIG=<home>/config.json <launcher> -port <N> -no-browser`. Do not
save settings in that launcher: a save rewrites the instance's config.

## Several instances

With `KQ_SANDBOX_MAX_INSTANCES` above 1, `instance` (the name) is required on every tool call
while more than one instance runs; the error lists them. It is optional while exactly one runs.
`sandbox_instance op=list` shows them, and `down`, `restart` and `logs` take `name`. Each instance
is a whole gateway process, and every start is checked against the supervisor's memory floor.

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
