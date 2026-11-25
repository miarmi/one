# ONE chat bridge

A small program for the owner's Mac. It reads the chats in Beeper Desktop, hands
them to ONE, and sends the chat replies the owner approved in ONE. It is not part
of the ONE app and is not built or shipped with it.

What it never does: log message text, names, chat ids or tokens; take a token
on the command line; talk to any Beeper address except this Mac; talk to any
ONE address except the one you give it; follow a redirect; turn on Beeper's
remote access.

## Set it up

1. In Beeper Desktop, turn on the local API and make an access token
   (Settings, Developers). Leave "remote access" off.
2. In ONE, open Connectors and copy the Bridge token.
3. Make the folder and the two token files, readable by you only:

   ```
   mkdir -p ~/.one-bridge && chmod 700 ~/.one-bridge
   pbpaste > ~/.one-bridge/one-token       # the Bridge token, just copied
   # put the Beeper token in ~/.one-bridge/beeper-token the same way
   chmod 600 ~/.one-bridge/*-token
   ```

   The program refuses a token file that others can read. The tokens may
   instead be in the environment (`BEEPER_TOKEN`, `ONE_BRIDGE_TOKEN`), or the
   files may be elsewhere (`BEEPER_TOKEN_FILE`, `ONE_BRIDGE_TOKEN_FILE`).
4. Build it (needs Go): `go build -o ~/.one-bridge/bridge ./apps/one/bridge`
5. Try one round: `ONE_URL=https://<your ONE address> ~/.one-bridge/bridge -once`
   It prints nothing on success; ONE's Connectors page then shows the chat
   bridge as heard just now.

## Settings (environment)

| Name | Meaning | Default |
| --- | --- | --- |
| `ONE_URL` | the one address of ONE, https | none, required |
| `BEEPER_URL` | Beeper Desktop on this Mac, localhost only | `http://localhost:23373` |
| `BRIDGE_STATE` | where it keeps its place | `~/.one-bridge/state.json` |
| `BRIDGE_POLL_SECONDS` | how often it looks for news | 20 |
| `BRIDGE_BACKFILL_DAYS` | how far back a chat is read the first time, until ONE says otherwise in its answers | 30 |
| `BEEPER_TOKEN`, `ONE_BRIDGE_TOKEN` | the two tokens, instead of the files | none |
| `BEEPER_TOKEN_FILE`, `ONE_BRIDGE_TOKEN_FILE` | where the two token files are | `~/.one-bridge/beeper-token`, `~/.one-bridge/one-token` |

## Start it at login

Save as `~/Library/LaunchAgents/one.bridge.plist`, with your ONE address, then
`launchctl load ~/Library/LaunchAgents/one.bridge.plist`:

```
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>one.bridge</string>
  <key>ProgramArguments</key><array><string>/Users/YOU/.one-bridge/bridge</string></array>
  <key>EnvironmentVariables</key><dict><key>ONE_URL</key><string>https://YOUR-ONE-ADDRESS</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>/Users/YOU/.one-bridge/bridge.log</string>
</dict></plist>
```

## See that it runs

- ONE, Connectors, "Chat bridge": when it was last heard, and how many chats
  and messages it has brought. If the Mac is off for more than the set time,
  ONE shows a strip and sends one notice.
- `~/.one-bridge/bridge.log` holds counts and fixed words only.

## How it keeps from sending twice

- It remembers a short fingerprint of every message ONE has taken, in the state
  file, and writes it only after ONE answered. A restart sends nothing again;
  an edited message has a new fingerprint and goes once more.
- A reply is written to the state file as "sending" before it leaves. If the
  program stops in between, it tells ONE "may have gone" and never sends it
  again. A reply it could not report is reported on the next round, never sent
  again.

## Files and the owner's own writing

- A file in a chat is fetched with Beeper's download call, read from the local
  path it names and sent to ONE as a stream, within the size ONE states. One
  that cannot be fetched is reported as pending and tried again, never dropped.
- What the owner writes in an open chat row comes in the outbox as parts: the text,
  then each file as its own message, in order. Each file is fetched from ONE with
  the bridge's own token, uploaded to Beeper and sent. A part Beeper reports as
  failed for good is told to ONE as failed with the reason; one still going out
  is not sent again.

## What it follows in Beeper

`beeper.go` follows Beeper Desktop's documented local API: `/v1/accounts` (a
plain list), `/v1/chats`, `/v1/chats/{id}/messages` with cursor and direction
and `hasMore`, `/v1/assets/download`, `/v1/assets/upload` and sending. The tests
use a fake Beeper that is at least as strict as the documentation.

## Not yet checked on a real Mac

- The exact status words Beeper gives for a network that needs a new login.
- Downloading and uploading a real file (large ones, and the local path shape).
- Sending to a chat that does not exist yet (the returned chat id).
- How long a message stays pending before Beeper says sent or failed.
- Edits and deletions as Beeper really reports them.
- The first run's one-month backfill on a real, busy account.

Run the tests with `go test ./apps/one/bridge`.
