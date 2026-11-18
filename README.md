# muster-plugin-template

A starting point for a [muster](https://github.com/ZacxDev/muster) plugin, in
Go, with no dependencies beyond the standard library.

A **plugin** is a small program you run that connects muster to another
system, such as an inbox or a chat. It brings that system's messages to your
muster **chief** (or to an officer), and sends back the replies you approve.
muster never calls the plugin: the plugin calls muster for everything.

This template does the whole round-trip against a **fake** system, so you can
see it work before you write any real integration:

1. Every `ECHO_INTERVAL` a fake upstream produces a message (`echo-1`,
   `echo-2`, …) and the plugin posts it to muster (`POST /plugin/v1/messages`).
2. muster hands it to your chief. On Kubernetes the chief can draft a reply
   itself; on the Docker Compose install it hands the message to one of your
   mirrored Claude Code sessions, and that session writes the reply.
3. You approve the draft in muster (**Asks → Drafts**).
4. The plugin claims the approved draft (`POST /plugin/v1/drafts/claim`),
   "sends" it by printing it to stdout, and reports the result
   (`POST /plugin/v1/drafts/{id}/result`).

New to muster plugins? Read muster's
[plugin quickstart](https://github.com/ZacxDev/muster/blob/main/docs/plugins-quickstart.md)
first; the full API is [docs/plugins.md](https://github.com/ZacxDev/muster/blob/main/docs/plugins.md).

## Run it against a local muster

**Tested against** muster commit `be827a860` (after release `v0.246.0`): the
whole run below, from the plugin's first message to its `SEND` and
`muster recorded sent`, with the settings this section names and a
`muster-mirror` set up as below. The chief's choice to route, and the Claude
Code session's answer, were played by test stand-ins calling the same tools.

You need Go (the version in `go.mod`) and a muster you can sign in to. A
local install answers at `http://localhost:8105`. Something has to write the
reply, so you also need what the quickstart's
[Before you start](https://github.com/ZacxDev/muster/blob/main/docs/plugins-quickstart.md#before-you-start) lists:

- **A chief with a credential**: the `chief` profile and a Claude
  subscription token (`claude setup-token`) or an OpenRouter key
  ([features page, Chief](https://github.com/ZacxDev/muster/blob/main/docs/features.md#chief)).
- **On the Docker Compose install, one mirrored Claude Code session** with
  muster's tools. The Compose chief cannot draft a reply itself: it routes
  the message to that session, and the session writes the reply. On
  Kubernetes the chief drafts it, and you can skip this.

  **The one-line install does not set this session up.** Its `muster-mirror`
  is read-only, and a read-only host refuses the route: the chief's
  `route_to_session` answers `host_not_interactive` ("This host is read-only
  — its muster-mirror was not started with -interactive."). Do this once, on
  the machine where the session runs (the quickstart's
  [The session that writes the reply](https://github.com/ZacxDev/muster/blob/main/docs/plugins-quickstart.md#the-session-that-writes-the-reply)
  explains each step):

  1. Add two lines to `~/.config/muster-mirror/env`:

     ```sh
     MUSTER_MIRROR_INTERACTIVE=1                # muster may type into this host's windows: effectively a shell here
     MUSTER_MIRROR_MCP_LISTEN=127.0.0.1:47823   # muster's tools for this host's Claude Code sessions
     ```

  2. Restart it: `systemctl --user restart muster-mirror` on Linux. On macOS
     run the installer again instead, as
     `curl -fsSL https://github.com/ZacxDev/muster/releases/latest/download/install.sh | MUSTER_MIRROR_INTERACTIVE=1 sh`
     (its service keeps a copy of the settings from the last install). The
     log says `INTERACTIVE (-interactive)` and
     `MCP endpoint for this host's Claude Code sessions at http://127.0.0.1:47823/mcp`.
  3. Register the endpoint with Claude Code by running the first line, and
     add the second to the shell tmux starts (`~/.zshrc` or `~/.bashrc`):

     ```sh
     claude mcp add-json --scope user muster '{"type":"http","url":"http://127.0.0.1:47823/mcp","headers":{"Authorization":"Bearer ${MUSTER_MIRROR_MCP_SECRET:-}","X-Muster-Pane":"${TMUX_PANE:-}","X-Muster-Tmux":"${TMUX:-}"}}'
     export MUSTER_MIRROR_MCP_SECRET="$(cat ~/.config/muster-mirror/mcp-secret 2>/dev/null)"
     ```

  4. Start Claude Code in a new tmux window. Its `/mcp` lists `muster`, with
     `route_reply` among its tools.

1. **Turn plugins on**, as in the quickstart's
   [step 1](https://github.com/ZacxDev/muster/blob/main/docs/plugins-quickstart.md#1-turn-plugins-on). In the `.env` beside muster's
   `docker-compose.yml` (`~/muster` for the one-line install) add

   ```sh
   MUSTER_PLUGINS=1
   MUSTER_SESSION_ROUTING=1   # Compose: lets the chief hand the message to your session
   ```

   then run `docker compose up -d --wait` there. muster's log says
   `plugins: ENABLED (MUSTER_PLUGINS=1)` and `session routing: ON`.
2. **Create the plugin.** Open `http://localhost:8105/plugins` (it has no menu
   link), give it a name such as `example-echo` and a label such as `Echo`,
   and press **Create plugin token**. Copy the `mpl_…` token: it is shown once.
3. **Configure and start the plugin:**

   ```sh
   cp .env.example .env
   # edit .env: MUSTER_URL=http://localhost:8105, MUSTER_PLUGIN_TOKEN=<the token>, ECHO_INTERVAL=1m
   set -a; . ./.env; set +a
   go run .
   ```

   It logs the plugin's name and capabilities, then
   `message echo-1 stored as <id> (queued)`.
4. **Watch the message reach a writer**, as in the quickstart's
   [step 4](https://github.com/ZacxDev/muster/blob/main/docs/plugins-quickstart.md#4-watch-the-chief-handle-it). Each message reaches your chief
   in a conversation named after the plugin's label, marked as untrusted data.
   - **On Compose** the chief routes it to your mirrored session
     (`route_to_session`). It is queued in that session's window and pasted
     once Claude Code is idle; the session answers with its `route_reply`
     tool, and the answer becomes the reply draft. If the chief leaves it, ask
     it in its chat to route the echo message to your session. If it reports
     the route refused with `host_not_interactive`, that host's
     `muster-mirror` is still read-only: do the setup above.
   - **On Kubernetes** the chief can draft the reply itself (`reply_draft`).

   Without a chief the messages wait, queued, on `/plugins`.
5. **Approve the reply** on the **Asks** page's **Drafts** tab
   (`/attention?tab=drafts`): **Approve and send**.
6. Within a few seconds the plugin prints

   ```
   SEND draft <id> (reply to echo-1):
   <the reply you approved>
   ```

   and logs `draft <id>: muster recorded sent`. The draft's card moves to
   **Archived** as sent.

Stop it with Ctrl-C. Keep `STATE_DIR` between runs (below).

## Configuration

Read from the environment only; `.env.example` lists them.

| variable | required | meaning |
|---|---|---|
| `MUSTER_URL` | yes | muster's address. `https://`, or `http://` to `localhost`, `127.0.0.1` or `[::1]` only; the address alone: no user, path, query or fragment |
| `MUSTER_PLUGIN_TOKEN` | yes | the `mpl_…` token from `/plugins`. Never logged |
| `ECHO_INTERVAL` | no | how often the fake upstream produces a message; default `30s` |
| `STATE_DIR` | no | where the sent-drafts journal and the upstream cursor live; default `./state` |

## What it does on each answer

| call | answer | the plugin |
|---|---|---|
| any | `401` | stops: the token was revoked, or its owner disabled |
| any | `404` | stops: plugins are off on that server (or, on a result, logs: not a draft it claimed) |
| message | `202` / `200` | moves the cursor on and writes it to `STATE_DIR/echo-cursor` (`200` is a repeat muster already stored) |
| message | `400` | logs and drops it: sending it again cannot help |
| message | `409` | muster stored nothing (the officer the plugin delivers to is not defined): waits 5 s, doubling up to 5 min, and sends the **same** message again; the cursor does not move |
| message | `429` | waits `Retry-After`, then sends the **same** message again |
| message | network error, `5xx` | waits 5 s and sends the same message again |
| claim | `204` | claims again (each claim waits up to 25 s) |
| claim | `200` | journals the draft, sends it once, reports the result (none when the send's outcome is unknown: below) |
| result | network error, `5xx` | posts the same result again (a repeat is safe) |
| result | `409` | logs it: the draft already has the other result |

**At most once.** Before sending a draft it writes the draft id to
`STATE_DIR/journal.jsonl` and fsyncs it. A draft already in the journal is
never sent again: if it was sent, only its result is reported again; if a
crash left it claimed but not known to be sent, nothing is sent or reported,
and muster marks it unconfirmed after its 2-minute lease (it never offers it
again). This is muster's rule in
[plugins.md, "At most once"](https://github.com/ZacxDev/muster/blob/main/docs/plugins.md#at-most-once).

**Sent, not sent, or not known.** A sender returns one of three outcomes, and
each is reported differently:

| `Sender.Send` returns | the plugin reports | muster then |
|---|---|---|
| the upstream's id, no error | `"ok": true` with it as `external_ref` | marks the draft sent |
| an error wrapping `plugin.ErrUnknown` | **nothing** | marks it unconfirmed when its lease lapses; never offers it again, and you decide |
| any other error | `"ok": false` with the error as `detail` | hands the reply back to whoever wrote it to try again |

`"ok": false` means "known **not** sent". Return it only when the upstream
said so (a refusal, an error before anything left). A timeout after the
request went out, or a connection lost mid-send, is `ErrUnknown`: reporting
`ok: false` then could get the same reply sent twice.

**Long sends.** muster leases a claimed draft for 2 minutes. A sender that
can take longer (a post made through a browser, say) should renew the lease
about once a minute while it runs, with
[`POST /plugin/v1/drafts/{id}/renew`](https://github.com/ZacxDev/muster/blob/main/docs/plugins.md#post-pluginv1draftsidrenew-keep-a-long-sends-claim),
up to 10 minutes after the claim. This template does not renew: its "send" is
a print. Without a renewal a slow success is marked unconfirmed under it.

**The journal grows.** `journal.jsonl` gains up to three short lines per draft
and is never compacted (a few megabytes after tens of thousands of drafts). Do
not delete it: a draft id missing from it is one the plugin would send again.
To shrink it, stop the plugin and keep only the last line for each
`draft_id`.

## Token scope

The token this template needs has the `messages` capability, the default on
`/plugins`. It opens muster's `/plugin/v1/*` routes and nothing else, and it
acts as you, the owner who minted it: it can post messages to your chief,
collect the replies **you approved**, and report what happened to them. It
cannot read your sessions, approve anything, or call any other muster route.
Revoke it on `/plugins` at any time; the plugin then stops at its next call.

## Telemetry

None. The plugin talks only to the muster at `MUSTER_URL`.

## Make it your own plugin

1. **Use this template** on GitHub (or clone it) into a repository of your own.
2. Change the module path in `go.mod` and every import
   (`github.com/ZacxDev/muster-plugin-template/…`) to yours, and rename the
   binary (the repository directory, `.gitignore`'s `/muster-plugin-template`,
   and the `muster-plugin-template:` prefix of `main.go`'s log lines).
3. Replace `internal/echo`, and the two lines of `main.go`'s `run` that wire
   it in: `echo.OpenUpstream(cfg.StateDir)` and
   `&echo.Sender{Out: os.Stdout}`. Rename `ECHO_INTERVAL` too (in `main.go`
   and `.env.example`) if your upstream is not polled on a timer.
   - `Upstream` becomes a reader of your system (`plugin.Upstream`): `Next`
     returns the next message, the same one until `Advance`, and `Advance`
     moves your cursor on durably. Give every message a stable `external_id`
     (muster's idempotency key). The runner calls `Advance` only after muster
     answers `2xx` (or a `400`).
   - `Sender` becomes the call that delivers a reply there (`plugin.Sender`).
     Return the upstream's id for what you sent; it is reported as
     `external_ref`. Wrap `plugin.ErrUnknown` when you cannot tell whether it
     went (above).
4. Set the message fields your system knows: `sender_verified` only if you
   authenticated the sender, `no_reply` for a message nobody can answer,
   `thread` for a thread id
   ([the message fields](https://github.com/ZacxDev/muster/blob/main/docs/plugins.md#post-pluginv1messages-an-inbound-message)).
5. Pick the capabilities you need. `messages` is enough for this round-trip;
   `draft_checks` lets the plugin refuse a reply its system cannot carry before
   you are asked to approve it
   ([capabilities](https://github.com/ZacxDev/muster/blob/main/docs/plugins.md#capabilities)).
6. State your token scope and your telemetry in your README, as this one does,
   and add your plugin to muster's
   [ecosystem page](https://github.com/ZacxDev/muster/blob/main/docs/ecosystem.md).
   Replace this README's **Tested against** line with the muster release (or
   commit) you ran your plugin against, and update it each time you test
   again: the listing asks for it.
7. Keep the secret scan. CI runs gitleaks over the whole history with
   `.gitleaks.toml`, which allows one exact test fixture (`mpl_s3cretT0ken`)
   and nothing else; remove that entry if you remove the fixture.

## Layout

| path | what |
|---|---|
| `main.go` | reads the environment, starts the loops |
| `internal/muster` | the client for muster's plugin routes |
| `internal/plugin` | the two loops: messages in, approved drafts out |
| `internal/echo` | the fake upstream (with its cursor) and the stdout "sender": replace these |
| `internal/journal` | the sent-drafts journal |
| `internal/mustertest` | a fake muster for tests (`httptest`) |

## Tests

```sh
go test ./...
```

The tests run against `internal/mustertest`, a fake muster that answers the
four routes this plugin calls the way muster does. CI runs gitleaks, `gofmt`,
`go vet` and `go test -race` on every push.

## Licence

MIT. See [LICENSE](LICENSE).
