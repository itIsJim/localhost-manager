# local-port-manager

A tiny local web UI to see which TCP ports are listening on your machine, what
state they're in, and to kill the processes holding them. Zero dependencies —
just Node.js and the system `lsof`/`ps` (macOS or Linux).

## Run

```sh
npm start          # or: node server.js
```

Then open <http://localhost:4321>.

Environment variables:

| Variable      | Default | Meaning                                  |
| ------------- | ------- | ---------------------------------------- |
| `PORT`        | `4321`  | Port the manager UI itself listens on    |
| `STALE_HOURS` | `24`    | Uptime threshold before a port is stale  |

## Statuses

- **active** — listening and currently in use (has established connections).
- **pending** — listening but idle: nothing is connected to it yet. Typical
  for dev servers that are still compiling / warming up, or just not visited.
- **stale** — the owning process has been up for more than 24 h, or the port
  is stuck: it shows as LISTEN but a TCP health probe can't actually connect
  (e.g. the process is wedged on an error).

Each row shows the reason for its status under the badge.

## Killing

The **Kill** button sends `SIGTERM`; if the process survives, you're offered a
`SIGKILL` follow-up. Safety rails:

- Only processes owned by your own user can be killed.
- The manager won't kill itself (its own row is marked "this app").
- The server double-checks that the PID still holds a listening port before
  sending any signal.

The server binds to `127.0.0.1` only, so nothing is exposed to the network.
