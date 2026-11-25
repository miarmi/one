# Lab with ONE

A small self-hosted "lab": one server that runs small Go apps, a GitHub Actions
pipeline that tests every change before it reaches production, and one real app
on top of it, **ONE**: a single queue for your private mail, chats and
calendars, reviewed by Claude, where nothing is sent or changed without your
Approve. A tiny `hello` app is included as the smallest example of an app.

This is a template. Nothing in it is configured for anybody yet.

## What you need

- A server at a provider like Hetzner (a small cloud server with Ubuntu is
  enough for the lab; the pipeline's test server is a second, separate one, or
  you use GitHub's own runners, see below).
- A domain name you can point at the server (the apps live on names below it).
  Without one the lab runs on plain `sslip.io` addresses over HTTP, which is
  only for trying it out.
- A GitHub repository of your own that holds this code (create it yourself from
  this template; it can be private).
- Go (the version in `go.mod`) to build and test on your own machine.
- For ONE's review: a Claude API key (optional; ONE works without the review).

## Set it up

Follow the documents that are already in the repository; this README does not
repeat them:

1. Edit `lab.json`: `domain`, `server_ip`, `repo` (`OWNER/REPO` of your GitHub
   repository) and `allow_ips` (who may open the apps). Everything else reads
   these values.
2. `platform/cloudinit/build.py` builds the server's first-boot setup text
   (read the header of that file). `platform/seed/seed.py` is the program that
   text installs. `platform/runner/build.py` does the same for the test
   server; to use GitHub's hosted runners instead, set the repository variable
   `LAB_RUNNER` to `ubuntu-24.04`.
3. `platform/CONTRACT.md` is the reference for how the pipeline, the server
   program (labd) and apps fit together, including the login gate and its
   one-time setup code (`platform/scripts/new-setup-code.sh`).
4. `CLAUDE.md` holds the working rules (spec first, tests decide what ships).
   It is written for the agents that build in this repository.

## ONE and its connections

ONE keeps its own data on the lab server and fetches by itself. There is no
configuration file for it: the owner opens the **Connectors** page in ONE and
enters what each source needs:

- Gmail (mail), a Microsoft work mailbox and calendar (through Microsoft Graph),
  Google and iCloud calendars: the keys, app passwords or sign-ins that the page
  asks for, one section per source.
- Claude: the API key, so that ONE can review what came in.
- Tokens for agents (a Claude connector, an optional "work tool" that may read
  one ready list) are made and revoked on the same page.
- Chats (WhatsApp, Messenger, iMessage, SMS) come through an **optional** small
  program, the chat bridge, that runs on a Mac next to Beeper Desktop and hands
  the chats to ONE. Without it ONE simply has no chats. See
  `apps/one/bridge/README.md`.

`apps/one/SPEC.md` is what ONE must do, `apps/one/API.md` the agent interface,
`apps/one/DESIGN.md` the design notes.

Not included: the push-notification app that the original lab used. labd and
`appkit.Notify` still try to deliver notifications through an app named
`notify`; without one they log and drop the message.

Times: ONE's default time zone is Europe/Prague (`apps/one/handlers.go`);
change it there if you live elsewhere.

## No warranty

This software is provided as it is, without warranty of any kind (see
`LICENSE`). It runs a server that is reachable from the internet and handles
your mail and chats: read `platform/CONTRACT.md`, keep the server updated and
use it at your own risk.
