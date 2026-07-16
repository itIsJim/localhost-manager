# localhost-manager

<img width="1097" height="411" alt="screenshot" src="https://github.com/user-attachments/assets/06fa7eec-fb10-47f5-81eb-dcd38e95245d" />


A tiny web UI **and terminal UI** that answer "what is running at
`localhost:<port>`?" — and let you kill it. Written in Go; scanning uses the
system `lsof`/`ps` (macOS or Linux). The web server is stdlib-only; the
terminal UI is built on [Bubble Tea](https://github.com/charmbracelet/bubbletea).

## Run

Requires Go 1.24+.

```sh
go build -o localhost-manager . && ./localhost-manager
```

Then open <http://localhost:4321>.

The same binary is also a terminal client — no server needed:

```sh
localhost-manager tui          # interactive TUI: browse, filter, kill
localhost-manager list         # print the port table once (pipe-friendly)
localhost-manager kill 3000    # SIGTERM whatever holds port 3000 (--force → SIGKILL)
```

In the TUI: `↑/↓`/`j/k` move, `x` kills the selected process (confirm prompt;
offers SIGKILL if it survives SIGTERM), `/` filters as you type, `tab`/`1–4`
switch status filters, `r` refreshes (also auto-refreshes every 5 s), `q` quits.

Environment variables:

| Variable      | Default | Meaning                                  |
| ------------- | ------- | ---------------------------------------- |
| `PORT`        | `4321`  | Port the manager UI itself listens on    |
| `STALE_HOURS` | `24`    | Uptime threshold before a port is stale  |

## What it shows

Only listeners actually reachable at `localhost:<port>` (loopback or wildcard
binds) — things bound solely to external interfaces are ignored. Each port is
a clickable `localhost:<port>` link, and the manager HTTP-probes every port to
identify what's serving it (page `<title>`, `Server` header, or redirect
target), so a row reads like:

> `localhost:3001` · com.docker.backend · **HTTP 200 · Gitea: Git with a cup of tea**

The table live-filters as you type (port, process name, PID, …), can be
narrowed to one status, and auto-refreshes every 5 seconds.

## Statuses

- **active** — listening and currently in use (has established connections).
- **pending** — listening but idle: nothing is connected to it yet. Typical
  for dev servers that are still compiling / warming up, or just not visited.
- **stale** — the owning process has been up for more than 24 h, or the port
  is stuck: it shows as LISTEN but a TCP probe can't actually connect
  (e.g. the process is wedged on an error).

Each row shows the reason for its status under the badge. IPv6-only listeners
(`[::1]`) are probed on the address they're actually bound to.

## Killing

The **Kill** button (web), `x` (TUI), and `kill <port>` (CLI) all send
`SIGTERM`; if the process survives, you're offered a `SIGKILL` follow-up.
Safety rails:

- Only processes owned by your own user can be killed.
- The manager won't kill itself (its own row is marked "this app").
- The server double-checks that the PID still holds a listening port before
  sending any signal.

## Security

- The server binds to `127.0.0.1` only, so nothing is exposed to the network.
- Requests with a non-loopback `Host` header are rejected, which blocks DNS
  rebinding.
- Write requests must be `Content-Type: application/json` (so browsers force a
  CORS preflight) and any `Origin` header must be loopback — a random webpage
  you visit can't POST to `/api/kill`.
