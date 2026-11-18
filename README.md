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
2. muster hands it to your chief, which may draft a reply.
3. You approve the draft in muster (**Asks → Drafts**).
4. The plugin claims the approved draft (`POST /plugin/v1/drafts/claim`),
   "sends" it by printing it to stdout, and reports the result
   (`POST /plugin/v1/drafts/{id}/result`).

New to muster plugins? Read muster's
[plugin quickstart](https://github.com/ZacxDev/muster/blob/main/docs/plugins-quickstart.md)
first; the full API is [docs/plugins.md](https://github.com/ZacxDev/muster/blob/main/docs/plugins.md).

## Run it in five minutes against a local muster

You need Go (the version in `go.mod`) and a muster you can sign in to. A
local install answers at `http://localhost:8105`.

1. **Turn plugins on.** In the `.env` beside muster's `docker-compose.yml`
   (`~/muster` for the one-line install) add `MUSTER_PLUGINS=1`, then run
   `docker compose up -d --wait` there. muster's log says
   `plugins: ENABLED (MUSTER_PLUGINS=1)`.
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
4. **Watch the chief.** Each message reaches your chief in a conversation named
   after the plugin's label, marked as untrusted data. The chief can draft a
   reply. (Without a chief the messages wait, queued, on `/plugins`; turning
   the chief on is in muster's
   [features page](https://github.com/ZacxDev/muster/blob/main/docs/features.md#chief).)
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
| `MUSTER_URL` | yes | muster's address. `https://`, or `http://` to `localhost`, `127.0.0.1` or `[::1]` only; no user, query or fragment |
| `MUSTER_PLUGIN_TOKEN` | yes | the `mpl_…` token from `/plugins`. Never logged |
| `ECHO_INTERVAL` | no | how often the fake upstream produces a message; default `30s` |
| `STATE_DIR` | no | where the sent-drafts journal lives; default `./state` |

## What it does on each answer

| call | answer | the plugin |
|---|---|---|
| any | `401` | stops: the token was revoked, or its owner disabled |
| any | `404` | stops: plugins are off on that server (or, on a result, logs: not a draft it claimed) |
| message | `202` / `200` | moves to the next message (`200` is a repeat muster already stored) |
| message | `400` | logs and drops it: sending it again cannot help |
| message | `429` | waits `Retry-After`, then sends the **same** message again |
| message | network error, `5xx` | waits 5 s and sends the same message again |
| claim | `204` | claims again (each claim waits up to 25 s) |
| claim | `200` | journals the draft, sends it once, reports the result |
| result | network error, `5xx` | posts the same result again (a repeat is safe) |
| result | `409` | logs it: the draft already has the other result |

**At most once.** Before sending a draft it writes the draft id to
`STATE_DIR/journal.jsonl` and fsyncs it. A draft already in the journal is
never sent again: if it was sent, only its result is reported again; if a
crash left it claimed but not known to be sent, nothing is sent or reported,
and muster marks it unconfirmed after its 2-minute lease (it never offers it
again). This is muster's rule in
[plugins.md, "At most once"](https://github.com/ZacxDev/muster/blob/main/docs/plugins.md#at-most-once).

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
   binary (the repository directory, `.gitignore`'s `/muster-plugin-template`).
3. Replace `internal/echo`:
   - `Upstream.Next` becomes a reader of your system. Give every message a
     stable `external_id` (muster's idempotency key), and advance your own
     cursor only after muster answers `2xx`.
   - `Sender.Send` becomes the call that delivers a reply there. Return the
     upstream's id for what you sent; it is reported as `external_ref`.
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

## Layout

| path | what |
|---|---|
| `main.go` | reads the environment, starts the loops |
| `internal/muster` | the client for muster's plugin routes |
| `internal/plugin` | the two loops: messages in, approved drafts out |
| `internal/echo` | the fake upstream and the stdout "sender": replace these |
| `internal/journal` | the sent-drafts journal |
| `internal/mustertest` | a fake muster for tests (`httptest`) |

## Tests

```sh
go test ./...
```

The tests run against `internal/mustertest`, a fake muster that answers the
four routes this plugin calls the way muster does. CI runs `gofmt`, `go vet`
and `go test -race` on every push.

## Licence

MIT. See [LICENSE](LICENSE).
