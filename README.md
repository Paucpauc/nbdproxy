# nbdproxy

English | [Русский](README_RU.md) | [简体中文](README_CN.md) | [Español](README_ES.md) | [Français](README_FR.md)

A small, dependency-free NBD proxy that keeps the **local client socket open**
while reconnecting to a restarted backend. Works with TCP and Unix sockets on
both sides. Linux `nbd-client` does not need `-persist` or netlink reconnect
support, including versions such as 3.26.1.

```text
Linux /dev/nbd0 ── stable local socket ── nbdproxy ── reconnect ── NBD server
```

Designed for a local server such as s3nbd that is restarted independently of the
proxy. One process exposes one configured export to **one client connection**.
All requests execute in wire order, with at most one outstanding backend request.
This deliberately trades throughput for straightforward replay ordering.

## Build and run

Requires Go 1.24 or newer; no external modules or C libraries.

```sh
go build -trimpath -o nbdproxy .
./nbdproxy -version
sudo install -d -m 0755 /run/nbdproxy
sudo ./nbdproxy \
  -listen unix:/run/nbdproxy/nbd.sock \
  -upstream tcp:127.0.0.1:10809 \
  -export disk1
```

In another terminal on Linux:

```sh
sudo modprobe nbd
sudo nbd-client -unix /run/nbdproxy/nbd.sock /dev/nbd0 -N disk1 -b 4096
```

Use **one connection**, without `-C 2`. `-persist` is unnecessary for backend
restarts. Avoid a finite kernel NBD I/O timeout if outages can exceed it; a kernel
request timeout can still fail I/O even though the local socket remains open.
On an existing device, inspect `/sys/block/nbd0/queue/io_timeout` (milliseconds,
if exposed by your kernel). A zero `nbd-client -timeout` leaves the kernel's
existing/default setting unchanged; it is not a reliable way to reset a timeout
previously set on a reused device. Verify your kernel's effective setting.

TCP frontend or Unix backend examples:

```sh
./nbdproxy -listen tcp:127.0.0.1:10810 -upstream unix:/run/s3nbd/nbd.sock -export disk1
sudo nbd-client 127.0.0.1 10810 /dev/nbd0 -N disk1 -b 4096
```

Neither leg uses authentication or TLS. Keep TCP listeners on loopback or a
trusted, restricted network. The frontend Unix socket is created with mode 0600.
The parent directory must already exist. Existing socket paths are not blindly
removed at startup; investigate and remove stale sockets manually if necessary.
Go removes a Unix listener's socket on a normal close.

## What survives a restart

1. The proxy negotiates the configured export with the backend before completing
   client negotiation. Backend absence at initial attach delays negotiation, so
   initial `nbd-client` attachment can still time out and need to be retried.
2. During transmission, each request and its WRITE payload stay in memory until
   a complete backend reply has arrived. A partial READ reply is discarded.
3. On dial, negotiation, transport, or backend I/O timeout errors, the proxy waits
   and reconnects indefinitely. The local client socket stays open. Backend NBD
   error replies are forwarded normally, not retried.
4. On reconnect, backend size and transmission flags must match the original
   values exactly. A mismatch ends the client session rather than replaying onto
   an incompatible export. The configured address/name must still refer to the
   **same storage**: NBD does not provide a mandatory persistent export UUID, so
   size/flags/name checks cannot detect replacement with another same-size disk.
5. The pending request is replayed before any later request is submitted.

Replay is **at least once**, not exactly once. A WRITE may have completed before
its reply was lost. Repeating it is safe only when the old backend operation
cannot execute later, after newer operations. A stopped backend process meets
that condition; arbitrary network partitions, timeouts against a live/hung
server, and concurrent writers require backend fencing/serialization guarantees.
Do not use this as a generic distributed failover system or with another writer.

There is no journal or persistent write cache. A backend that acknowledges
volatile writes can lose them on a crash; the proxy cannot recover requests it
has already acknowledged to the kernel. Backend FLUSH/FUA durability still
matters. Replay also requires the backend's WRITE/TRIM implementation to be
safe to repeat. Restarting or killing **the proxy itself** breaks the client
connection and is not covered by backend reconnect support.

## Protocol and resource limits

- Fixed-newstyle NBD; frontend EXPORT_NAME, INFO, GO, LIST, ABORT.
- READ, WRITE, FLUSH, TRIM and negotiated FUA; simple replies only.
- Backend negotiation uses EXPORT_NAME with the configured name. Backends must
  accept that mode and its implicit block-size constraints (minimum 1 byte).
- Unsupported options (including structured replies and TLS) get ERR_UNSUP;
  unsupported commands/flags get EINVAL. Read-only exports reject writes.
- Multi-connection capability is masked even if the backend advertises it.
  A second frontend connection is closed without negotiation.
- READ/WRITE limit: 32 MiB per request. One executing request plus one reader
  lookahead; TCP/socket buffering supplies backpressure. Application payload
  buffers are bounded to approximately 96 MiB per active session (including a
  read reply), independent of outage duration. TRIM ranges carry no payload.
- Invalid magic, oversized options/WRITE payloads or a client disconnect end the
  session. A client that never finishes sending a request can occupy the sole
  session; this is a trusted local service, not a public multi-tenant endpoint.
- Backend failures log to stderr via Go's structured logger. A persistently
  invalid backend handshake/reply is retried; inspect logs to diagnose it.

Run `./nbdproxy -help` for options:

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `unix:/run/nbdproxy/nbd.sock` | Local listener |
| `-upstream` | `tcp:127.0.0.1:10809` | Backend address |
| `-export` | empty | Export name on both legs |
| `-retry` | `1s` | Fixed delay between reconnect attempts |
| `-connect-timeout` | `5s` | Backend dial timeout |
| `-handshake-timeout` | `30s` | Per-connection negotiation timeout |
| `-io-timeout` | `1m` | Backend request deadline and frontend response write deadline |

The backend request deadline triggers reconnect; it does not cap total recovery
time. The proxy applies no frontend read deadline during transmission, so idle
clients remain attached. SIGINT/SIGTERM closes the listener and active session,
cancels retries and exits. Unmount and disconnect NBD before stopping the proxy.

## systemd

Install the binary, edit the upstream/export in the supplied unit, and enable it:

```sh
sudo install -m 0755 nbdproxy /usr/local/bin/nbdproxy
sudo install -m 0644 packaging/nbdproxy.service /etc/systemd/system/nbdproxy.service
sudo systemctl daemon-reload
sudo systemctl enable --now nbdproxy
sudo journalctl -u nbdproxy -f
```

The unit uses DynamicUser, creates `/run/nbdproxy`, and needs no root privileges.
Root can connect `nbd-client` to its mode-0600 socket. A Unix backend must grant
access to the service user; for that deployment replace DynamicUser with a fixed
user/group and appropriate socket permissions. Keep this unit independent of the
backend service: do not add PartOf/BindsTo that stops the proxy on backend restart.
Restart=on-failure restarts the process but cannot preserve existing NBD sessions.

## Validation

```sh
go vet ./...
go test -race ./...
go test -run='^$' -fuzz=FuzzExportName -fuzztime=10s
```

Tests exercise committed writes with lost replies, partial read replies,
flush ordering, export changes, cancellation, malformed negotiation, request
validation and parser fuzzing. CI runs tests on Linux and macOS and builds all
release targets. The Linux job also attaches real `/dev/nbd0` using the distro
nbd-client, performs direct I/O, stops the backend, verifies an I/O stays pending,
restarts the backend and verifies data plus subsequent writes/flushes.

To run that destructive-to-device integration test manually, use a **disposable
Linux host with an unused `/dev/nbd0`** and install nbd-client:

```sh
sudo modprobe nbd nbds_max=4 max_part=0
go test -c -o /tmp/nbdproxy.test
sudo env NBDPROXY_KERNEL_TEST=1 /tmp/nbdproxy.test \
  -test.v -test.run '^TestLinuxKernelRestart$' -test.timeout 60s
```

The test writes to `/dev/nbd0`. It is opt-in and is skipped in ordinary test runs.
The real-kernel test cannot run on macOS.

## Releases

```sh
go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.41.0
./scripts/release.sh v0.1.0
```

Produces static Linux amd64/arm64 and macOS amd64/arm64 archives, each with the
binary, documentation, license and systemd unit, plus DEB and RPM packages for
Linux amd64/arm64 and SHA-256 checksums for all artifacts in `dist/`.
Packages install the binary in `/usr/bin` and the unit in `/usr/lib/systemd/system`;
configure the upstream with a systemd override before enabling the service.

After pushing this repository to GitHub, pushing a `v*` tag triggers the release
workflow. It vets and tests, builds the archives and packages, and immediately
publishes a GitHub Release with generated notes. CI and release
workflows have only read permissions except the release job's contents:write.

Protocol reference: [NBD specification](https://github.com/NetworkBlockDevice/nbd/blob/master/doc/proto.md).
License: MIT.
