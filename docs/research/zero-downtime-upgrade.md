# Zero-downtime process upgrade and self-update lifecycle

Research notes for go-binsync's "swap the binary, re-exec, hand off sockets, health-check, roll back" phase.
Scope: Linux, Go web servers, one host at a time. Sources are primary where possible (source code, man
pages, kernel docs, vendor docs); URLs inline. Where a claim is my inference rather than a citation it is
marked *(inference)*.

## 1. Summary of findings

- **The only handoff mechanism with zero connection loss on every kernel is sharing the same listening
  socket** (fd inheritance across `exec`, or `SCM_RIGHTS` over a Unix socket). Two processes calling
  `accept(2)` on dups of one socket share one accept queue; the kernel hands each connection to exactly one
  caller. nginx, tableflip, HAProxy 1.8+, Envoy and systemd's fd store all do this.
- **`SO_REUSEPORT` is not a handoff mechanism.** Separate sockets have separate accept queues; closing the
  old listener resets everything queued on it (established-in-accept-queue *and* mid-handshake). LWN
  documented the drop at introduction (3.9), HAProxy measured 155 failures/1M connections over 180 reloads,
  Cloudflare rejected it for tableflip. The kernel fix is `net.ipv4.tcp_migrate_req` (commit `f9ac779f881c`,
  first released in **v5.14**, default **0**), which migrates queued/in-flight sockets to a surviving listener
  on `close()`/`shutdown()`. Below 5.14 or with the sysctl off, REUSEPORT reloads drop connections.
- **tableflip** (Cloudflare) is the reference Go design: `exec(os.Args[0])` with fds 3/4 as ready/names
  pipes and passed files from fd 5, a gob-encoded name table, a single byte (42) for "ready", parent-exit
  detection by EOF on an fd the parent never closes, **`UpgradeTimeout` (default 1 min) after which the
  child is `SIGKILL`ed and the old process keeps running**, exactly one upgrade in flight, and the
  invariant "no old code runs after a successful upgrade". It refuses a second upgrade until the parent has
  exited (#52), does not preserve established connections (#74, by design), needs its own `StartProcess`
  wrapper because `exec.Cmd` clears `O_NONBLOCK` on passed fds (#60), and does not work on Windows.
- **nginx / unicorn** use the same dance with signals instead of a library API: `USR2` (exec new master,
  pid file renamed to `.oldbin`, fds passed in `NGINX=fd;fd;` env), `WINCH` (old workers drain),
  `QUIT` (old master exits). Rollback is symmetric: `HUP` old master + `QUIT` new, or `TERM` new and the
  old master respawns workers automatically. The old master *keeps its listen sockets* during the whole
  window, which is what makes rollback free.
- **HAProxy** master-worker (`-W`) re-execs the master on `SIGUSR2` with `-sf <old worker pids>` and
  `-x sockpair@` to fetch listeners via `SCM_RIGHTS`; if the new process fails to bind, the old workers
  resume listening. **Envoy** passes sockets over a Unix socket RPC keyed by `--restart-epoch`, drains for
  `--drain-time-s` (gradual by default), and hard-kills the parent after `--parent-shutdown-time-s`
  (default 900 s). Its supervisor script treats *any* abnormal child exit as fatal for the whole tree.
- **systemd's fd store** (`FileDescriptorStoreMax=` + `sd_pid_notify_with_fds(FDSTORE=1)`) gives a restart
  that loses no connections *without a parent process*: the listener sits in the accept queue while the new
  process starts. It is not an overlap: there is an accept pause of one process start, and there is no
  built-in fallback to the previous binary; `Restart=` + `StartLimitBurst` (default 5 in 10 s) bounds the
  crash loop and then leaves the unit failed.
- **Draining is protocol-specific.** HTTP/1.1: `Connection: close` (RFC 9112 §9.6, and close in stages to
  avoid an RST erasing the last response). HTTP/2: `GOAWAY`, ideally twice (RFC 9113 §6.8: first with
  last-stream-id 2^31-1, then the real id after ≥1 RTT). Go's `http.Server.Shutdown` closes listeners,
  closes idle conns, polls (≤500 ms) for in-flight requests, disables keep-alives, and **does not track
  hijacked connections (WebSockets)** — you must `RegisterOnShutdown`. Go's bundled http2 sends a single
  `GOAWAY`. gRPC `GracefulStop` "blocks until all the pending RPCs are finished".
- **The overlap window is correct, not a hazard**, when both processes accept from the same socket. With
  REUSEPORT the overlap is where you lose connections.
- **You cannot `open(O_WRONLY)` a running executable (ETXTBSY)**, but you can `rename(2)` over it: the old
  inode survives for the running process and `/proc/self/exe` then reads `…/app (deleted)`. Go's
  `os.Executable()` trims the suffix, so it returns the *path* (= the new binary). Re-exec for upgrade must
  use the path; re-exec of *yourself* must use `/proc/self/exe` (Teleport PR #11283 does exactly this).
  Go has an unfixed fork/exec `ETXTBSY` race (golang/go#22315) when the process that wrote the file also
  forks concurrently — retry with backoff.
- **Hardlink-then-rename** preserves the old binary with no window in which the path is missing
  (`link(app, app.old); rename(app.new, app)`), unlike go-update's rename-rename which has a window
  with no executable at the path. Rollback is one atomic `rename(app.old, app)`.
- **`memfd_create` + `execveat(AT_EMPTY_PATH)`** can exec a verified image without a disk race, but
  kernel ≥6.3 hosts may set `vm.memfd_noexec=2` (executable memfds refused), Go's `ExtraFiles` renumbering
  clobbers `/proc/self/fd/N` (golang/go#66654), and `/proc/self/exe` becomes `/memfd:… (deleted)`. Treat it
  as optional hardening, not the default path.
- **Update security needs more than a signature on the blob.** TUF's threat list (rollback, freeze,
  mix-and-match, endless data, arbitrary install) maps to a minimal design: signed manifest carrying
  `{name, version, sha256, size, created, expires}`, Ed25519 (minisign; its *trusted comment* is signed and
  documented as the place for version/filename "to prevent downgrade attacks"), monotonic version check,
  expiry check, size bound, verify the exact bytes you will exec.
- **Hook contracts converge** across systemd, dpkg, Kubernetes, Nomad: pre-hooks fail closed, post-hooks
  are informational, hooks are at-least-once and must be idempotent, each has a timeout, and the runner
  passes context by env vars (`$SERVICE_RESULT/$EXIT_CODE`, dpkg's `abort-upgrade` argument, Nomad's
  `fail_on_error`).

## 2. Socket handoff mechanisms on Linux

### 2a. fd inheritance across `exec`

The listening socket is a kernel object; a child created by `fork`+`exec` inherits every fd without
`FD_CLOEXEC`. The parent tells the child which fd numbers are what.

**nginx.** `ngx_exec_new_binary()` (`src/core/nginx.c`) builds `NGINX=<fd>;<fd>;…` from
`cycle->listening[i].fd`, renames the pid file to `<pid>.oldbin` (`NGX_OLDPID_EXT`), and execs `argv[0]`
with the current args; on rename failure it aborts the upgrade. The new master's
`ngx_add_inherited_sockets()` parses `getenv("NGINX")`, marks each `ls->inherited = 1`, and logs
"using inherited sockets from …". Source:
https://raw.githubusercontent.com/nginx/nginx/master/src/core/nginx.c (`ngx_exec_new_binary`,
`ngx_add_inherited_sockets`), `#define NGINX_VAR "NGINX"` in `src/core/nginx.h`.

**Go.** `os/exec.Cmd.ExtraFiles`: "entry i becomes file descriptor 3+i. ExtraFiles is not supported on
Windows" (https://raw.githubusercontent.com/golang/go/master/src/os/exec/exec.go). `net.FileListener(f)`
"returns a copy of the network listener corresponding to the open file f … Closing ln does not affect f,
and closing f does not affect ln" (https://raw.githubusercontent.com/golang/go/master/src/net/file.go) —
i.e. it dups, so the parent can close its `net.Listener` after exec without affecting the child. Gotcha
(tableflip PR #60): `exec.Cmd.Start()` calls `os.File.Fd()` on every `ExtraFiles` entry, which "clears
O_NONBLOCK from the underlying open file descriptor", shared by all dups of that open file description;
this "can lead to hard to diagnose hangs … Even more troubling, File.Close doesn't interrupt these syscalls
anymore." tableflip replaced `exec.Cmd` with a thin `syscall.StartProcess` wrapper
(https://github.com/cloudflare/tableflip/pull/60). go-binsync must do the same or set `O_NONBLOCK` back on
the dup before wrapping it.

**tableflip wire protocol** (https://raw.githubusercontent.com/cloudflare/tableflip/master/child.go,
`parent.go`): child env gets `TABLEFLIP_HAS_PARENT_7DIU3=yes`; fds are `[stdin, stdout, stderr, readyW,
namesR, passed…]`, so the child reads names from fd 4 (gob `[][]string`, each `[kind, network, addr]`),
maps passed files from fd 5 upward and sets close-on-exec on them, and at `Ready()` writes one byte (`42`)
to fd 3 and closes it. The parent keeps `namesW` open forever after success (`neverCloseThisFile`) so the
child's `io.Copy(Discard, rd)` returns only when the parent process dies — that is how `WaitForParent`
and the "parent hasn't exited" gate work.

**systemd socket activation** is the same convention standardised: fds start at 3
(`SD_LISTEN_FDS_START`), `$LISTEN_PID` must equal `getpid()` "to prevent unintended inheritance",
`$LISTEN_FDS` is the count, `$LISTEN_FDNAMES` optional names, fds have `FD_CLOEXEC` set, "If multiple
socket units activate the same service, the order of the file descriptors passed to its main process is
undefined", and fd-store fds "appear last with duplicates removed"
(https://man7.org/linux/man-pages/man3/sd_listen_fds.3.html). facebookarchive/grace (archived 2019) used
the same `LISTEN_FDS` convention for its `SIGUSR2` restart (https://github.com/facebookarchive/grace).

### 2b. `SO_REUSEPORT` and why it is not a handoff

LWN's introduction article (kernel 3.9, Tom Herbert): connections are distributed by a hash of the
4-tuple; all binders need the same effective UID; and the drawback: "If the number of listening sockets
bound to a port changes because new servers are started or existing servers terminate, it is possible that
incoming connections can be dropped during the three-way handshake"
(https://lwn.net/Articles/542629/). HAProxy's history: "the socket queues were independent and people have
progressively started reporting occasional RSTs being observed under load during an HAProxy reload" — at
55k conn/s and 10 reloads/s, "155 connection failures for one million connections after 180 reloads"
(https://www.haproxy.com/blog/truly-seamless-reloads-with-haproxy-no-more-hacks). Cloudflare: "new-but-not-
yet-accepted connections on the socket used by the old process will be orphaned and terminated by the
kernel" (https://blog.cloudflare.com/graceful-upgrades-in-go/).

Steering knobs that *mitigate* but do not fix it: `SO_ATTACH_REUSEPORT_CBPF`/`_EBPF` (UDP 4.5, TCP 4.6;
`BPF_PROG_TYPE_SK_REUSEPORT` with `bpf_sk_select_reuseport` since 4.19) pick the socket per packet;
`SO_INCOMING_CPU` pins a listener per RX queue (https://man7.org/linux/man-pages/man7/socket.7.html). A
BPF program can stop steering SYNs to the dying listener, but whatever is already queued there is still
lost when it closes — this is the "drain then close" userspace workaround LWN describes
(https://lwn.net/Articles/837506/).

The kernel fix: `net.ipv4.tcp_migrate_req` — "When a listener is closed, in-flight request sockets during
the handshake and established sockets in the accept queue are aborted. If the listener has SO_REUSEPORT
enabled, other listeners on the same port should have been able to accept such connections. This option
makes it possible to migrate such child sockets to another listener after close() or shutdown(). The
BPF_SK_REUSEPORT_SELECT_OR_MIGRATE type of eBPF program should usually be used to define the policy to pick
an alive listener. Otherwise, the kernel will randomly pick an alive listener only if this option is
enabled." Default 0. (https://www.kernel.org/doc/html/latest/networking/ip-sysctl.html). Commit
`f9ac779f881c` "net: Introduce net.ipv4.tcp_migrate_req." by Kuniyuki Iwashima, 2021-06-12; the entry is in
`Documentation/networking/ip-sysctl.rst` at tag v5.14 and absent at v5.13; KernelNewbies lists it under
5.14 (https://kernelnewbies.org/Linux_5.14, https://lwn.net/Articles/853637/). Practical rule: REUSEPORT
handoff is acceptable only on ≥5.14 with the sysctl on, and go-binsync cannot assume either on a remote host.
Related: `tcp_abort_on_overflow` (reset instead of retransmit when the accept queue is full; leave off).

### 2c. `SCM_RIGHTS` over a Unix socket

`unix(7)`: `SCM_RIGHTS` sends "a reference to an open file description", semantically a `dup(2)` into the
receiver; at most `SCM_MAX_FD` = 253 fds per message (EINVAL beyond); if the receive buffer is too small
the excess fds "are automatically closed in the receiving process" with `MSG_CTRUNC`; the same happens if
`RLIMIT_NOFILE` would be exceeded. `SCM_CREDENTIALS`/`SO_PEERCRED` give kernel-checked pid/uid/gid of the
peer (https://man7.org/linux/man-pages/man7/unix.7.html). Users:

- **HAProxy 1.8+**: `-x <unix_socket>` "connect to the specified socket and try to retrieve any listening
  sockets from the old process, and use them instead of trying to bind new ones"; needs `expose-fd
  listeners` on the stats socket, or in master-worker mode the master "will use automatically this option
  upon a reload with the 'sockpair@' syntax" (https://docs.haproxy.org/3.0/management.html).
- **Envoy**: the new process asks the old for listen sockets over a Unix domain socket RPC
  (`--socket-path`, default abstract `@envoy_domain_socket`, `--socket-mode` 600); stats/gauges are
  carried in shared memory keyed by `--base-id`
  (https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/hot_restart).
- **systemd fd store**: `sd_pid_notify_with_fds()` with `FDSTORE=1` (+`FDNAME=`, `FDSTOREREMOVE=1`,
  `FDPOLL=0`) (https://man7.org/linux/man-pages/man3/sd_pid_notify_with_fds.3.html).

The advantage over exec-inheritance: the two processes need not be parent/child, so an external
orchestrator (or systemd) can hold the sockets. The cost: a control-socket protocol and its auth.

### 2d. systemd fd store + `Restart=`: restart without a parent

systemd.io/FILE_DESCRIPTOR_STORE: the store lets "services restart seamlessly (for example to update them
to a newer version), without losing execution context, dropping pinned resources, terminating established
connections or even just momentarily losing connectivity". Fds come back via `$LISTEN_FDS/$LISTEN_FDNAMES`.
Any stored fd that reports `POLLHUP`/`POLLERR` "is automatically closed and removed" unless `FDPOLL=0`. The
store is discarded when the service is stopped, except per `FileDescriptorStorePreserve=` (`no`/`yes`/
`restart`). Caveat: because the store holds a dup, "peers on a connection socket uploaded this way will not
receive an automatic POLLHUP event anymore if the service code issues close()" — send `FDSTOREREMOVE=1`
first (https://systemd.io/FILE_DESCRIPTOR_STORE/). `Type=notify-reload` (`RELOADING=1` +
`MONOTONIC_USEC=`, then `READY=1`) makes `systemctl reload` synchronous; `RestartMode=direct` (v254) skips
the failed/inactive transition (https://man7.org/linux/man-pages/man5/systemd.service.5.html).

What this buys: no parent process, survives soft-reboot, no library in the app beyond `sd_notify`. What it
does not: there is no overlap (accept pauses for one process start; the listen backlog absorbs it), and no
notion of "previous binary" — `Restart=on-failure` restarts whatever is at `ExecStart=`. Start rate
limiting: `StartLimitIntervalSec=`/`StartLimitBurst=` default 10 s / 5, applies to "all kinds of starts
(including manual)", reset with `systemctl reset-failed`
(https://man7.org/linux/man-pages/man5/systemd.unit.5.html).

### 2e. `pidfd` and credentials

`pidfd_open` (5.3) returns an fd that becomes readable "when the process terminates", usable with
`poll/epoll`, and is immune to PID reuse (`pidfd_send_signal` targets the fd)
(https://man7.org/linux/man-pages/man2/pidfd_open.2.html). systemd now exports `$LISTEN_PIDFDID`. For
go-binsync: watch the child with a pidfd rather than `waitpid` polling when the orchestrator is not the parent;
authenticate a control socket peer with `SO_PEERCRED` rather than a pid file.

## 3. Implementations

### 3.1 tableflip (Cloudflare)

Design goals from the blog: "No old code keeps running after a successful upgrade", "The new process can
crash during initialisation, without bad effects", "Only a single upgrade is active at any point in time"
(https://blog.cloudflare.com/graceful-upgrades-in-go/). API and state machine from
https://raw.githubusercontent.com/cloudflare/tableflip/master/upgrader.go:

```go
type Options struct {
    UpgradeTimeout time.Duration // default DefaultUpgradeTimeout = 1 minute
    PIDFile        string        // "The PID of a ready process is written to this file"
    ListenConfig   *net.ListenConfig
}
func New(opts Options) (*Upgrader, error)   // "Only the first call to this function will succeed"; ErrNotSupported on Windows
func (u *Upgrader) Listen(network, addr string) (net.Listener, error) // inherited or new (Fds)
func (u *Upgrader) Ready() error            // closes unused inherited fds, writes PID file (tempfile+rename), sends byte 42
func (u *Upgrader) Upgrade() error          // blocks until child ready / failed
func (u *Upgrader) Exit() <-chan struct{}   // closed when this process should exit
func (u *Upgrader) Stop()                   // no more upgrades; if before a successful upgrade, unlinks unix sockets
func (u *Upgrader) HasParent() bool
func (u *Upgrader) WaitForParent(ctx) error
```

`run()` loop, verbatim conditions: `Upgrade()` returns `"terminating"` after `Stop()`, `"already upgraded"`
after success, `"process is not ready yet"` if `Ready()` was not called, `"parent hasn't exited"` if this
process is itself a fresh child whose parent is still draining. `doUpgrade()`:

```
startChild(os.Args[0], os.Args[1:], fds=[stdin,stdout,stderr,readyW,namesR,passed...], env+sentinel)
loop:
  upgradeC      -> "upgrade in progress"
  child.result  -> "child %s exited[: err]"          (child crashed before Ready: parent continues)
  stopC         -> child.Kill(); "terminating"
  readyTimeout  -> child.Kill(); "new child %s timed out"   (SIGKILL after UpgradeTimeout)
  child.ready   -> success: stash namesW in exitFd (never closed), Fds.closeUsed(), close(exitC)
```

Sequence on success: parent closes its listener dups (`closeUsed`) and closes `exitC`; the application's
main goroutine sees `<-upg.Exit()`, calls `http.Server.Shutdown`, exits; the child observes EOF on fd 4
and `WaitForParent` returns. Limitations, from the issue tracker
(https://api.github.com/repos/cloudflare/tableflip/issues?state=all):

- #52 "Allow multiple back-to-back graceful upgrades": refused; while the old process drains long-lived
  connections a second upgrade is blocked ("parent hasn't exited"). Maintainer: the model is a chain of two
  processes; anything else "is reinventing systemd".
- #74 established connections are not preserved: "Tableflip is not meant to preserve established
  connections, only listeners (TCP) or unconnected sockets (UDP)"; do it yourself with `AddConn`+`SCM_RIGHTS`.
- #59 keeping the old process alive to forward signals "violates one of the invariants … after a
  successful upgrade, no old code is left running".
- #12 h2 clients saw `Unexpected EOF` on upgrade while h1 completed — GOAWAY vs `Connection: close`
  semantics differ (see §4).
- #60 `O_NONBLOCK` clobbering (above). #29/#63/#61: misuse and `argv[0]` bugs, fixed.
- Go cannot `fork()` without exec, so the nginx worker model (fork-only) is not available; `New()` allows
  a single global instance; `go run` breaks because `os.Args[0]` is a temp binary; Windows unsupported.
- systemd unit in the README: `ExecStart=/path/to/binary -some-flag /path/to/pid-file`,
  `ExecReload=/bin/kill -HUP $MAINPID`, `PIDFile=/path/to/pid-file`; journald could lose logs before
  v244 (systemd/systemd#13708).
- `Upgrade()` execs `os.Args[0]` verbatim: whatever path the shell/systemd used, resolved against the
  initial cwd. go-binsync must ensure that path is the installed path it swaps.

### 3.2 jpillora/overseer

Supervisor ("main") + worker; main binds `Config.Addresses` and passes them via `ExtraFiles`, env
`OVERSEER_IS_WORKER=1`, `OVERSEER_NUM_FDS`, `OVERSEER_BIN_ID` (SHA1 of binary); `Fetcher` (File/HTTP/
GitHub/S3) polls for a new binary. Notable validation step before swapping: it writes the candidate to a
temp path, chmod/chowns it, and runs it with `OVERSEER_BIN_CHECK=<token>`; the candidate must print the
token within 5 s, proving "this is an overseer-enabled binary that at least starts". `PreUpgrade(tempPath)`
hook can veto. Restart = send `RestartSignal` (SIGUSR2) to the worker, start the new worker, wait
`TerminateTimeout`, then `os.Kill`. Worker crash → main restarts it unless `NoRestart`. Warts: `init()`
runs in both processes, the main process's config cannot change without a full restart, binary move
"shells out to mv" (https://raw.githubusercontent.com/jpillora/overseer/master/README.md,
`proc_master.go`). The supervisor is itself unupgradable in place.

### 3.3 nginx / unicorn / puma

nginx control (https://nginx.org/en/docs/control.html): put the new binary in place; `USR2` → master
renames pid file to `.oldbin`, execs the new binary which starts new workers; both generations accept;
`WINCH` to old master → old workers exit gracefully; `QUIT` to old master → done. "the old master process
does not close its listen sockets". Rollback: "Send the HUP signal to the old master process. The old master
process will start new worker processes without re-reading the configuration. After that … QUIT signal to
the new master process", or `TERM` to the new master: "When the new master process exits, the old master
process will start new worker processes automatically" and "discards the .oldbin suffix". unicorn documents
the identical procedure (`USR2`, `.oldbin`, `WINCH`, `QUIT`; `HUP` old + `QUIT` new to back out)
(https://yhbt.net/unicorn/SIGNALS.html). puma distinguishes *hot restart* (`SIGUSR2`, `exec`, "Safe for
upgrading Puma itself", brief latency) from *phased restart* (`SIGUSR1`, cluster mode, replaces workers
one at a time, "Multiple application versions may run concurrently") (https://github.com/puma/puma/blob/master/docs/restart.md).

### 3.4 HAProxy master-worker

`-sf <pids>` sends SIGUSR1 (finish) to old processes "after boot completion", `-st` sends SIGTERM; `-W`
master-worker: "the master process reacts to the SIGUSR2 signal by reexecuting itself with the -sf
parameter followed by the PIDs of the workers"; `-Ws` for `Type=notify`. Safety: "if the new process
manages to bind correctly to all ports, then it sends either the SIGTERM … or the SIGUSR1 … to all
processes"; if bind fails the old workers "will then restart listening to the ports and continue to accept
connections". Soft stop = "only unbinding from listening ports, but continue to process existing
connections until they close"; `hard-stop-after` caps it (https://docs.haproxy.org/3.0/management.html).

### 3.5 Envoy hot restart

Sequence (https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/hot_restart): new
process fully initialises (config, service discovery, health checks) → requests listen socket copies from
the old → starts listening → tells the old to drain → old drains for `--drain-time-s` ("gradual" strategy
ramps the fraction of connections told to go away to 100%, or "immediate") → new tells old to shut down →
old dies no later than `--parent-shutdown-time-s` (default 900 s; "should exceed drain time"). Counters and
gauges are transferred (`NeverImport` excluded). `--restart-epoch` must be old+1; `--hot-restart-version`
must match; `--base-id`/`--use-dynamic-base-id` isolate instances. Caveats: existing connections are not
transferred; reducing `--concurrency` can drop connections in old accept queues; `socket_options` cannot
change across a hot restart; no Windows (https://www.envoyproxy.io/docs/envoy/latest/operations/cli).
`restarter/hot-restarter.py`: `SIGHUP` → `restart_epoch += 1`, `fork`+`exec` with `RESTART_EPOCH` env;
`SIGTERM/SIGINT` → TERM all children, 30 s, then KILL; `SIGCHLD` with non-zero/killed status →
`force_kill_all_children()` and exit 1. Note the asymmetry: Envoy's own design leaves the parent serving
if the child dies *before* taking the sockets, but the shipped supervisor gives up entirely on any
abnormal child exit.

### 3.6 Other

- **Caddy** reloads config in-process; binary upgrades are `caddy upgrade` + restart (not zero-downtime).
- **Kubernetes** rolling update replaces pods (`maxSurge`/`maxUnavailable`, readiness-gated) — the LB
  layer does the overlap; in-place upgrade is the single-host analogue where go-binsync must be the LB.
- **Nomad** `update` stanza: `max_parallel`, `health_check` (`checks`/`task_states`/`manual`),
  `min_healthy_time` 10 s, `healthy_deadline` 5 m, `progress_deadline` 10 m, `auto_revert`, `canary`,
  `auto_promote` (https://developer.hashicorp.com/nomad/docs/job-specification/update). The
  `min_healthy_time`/`healthy_deadline` pair is the right shape for a post-Ready probation window.
- **Tailscale** `clientupdate.updateLinuxBinary`: download tarball, extract to `.new` files, rename over,
  then `systemctl restart tailscaled.service`; refuses if installed ≥ latest; prefers distro package
  managers when present (https://raw.githubusercontent.com/tailscale/tailscale/main/clientupdate/clientupdate.go).
  Not zero-downtime; instructive for "verify then rename then restart".
- **Teleport `teleport-update`** (RFD 184): versions live in `/var/lib/teleport/versions/<ver>/` with a
  `sha256` sidecar written only after complete extraction (used as a completeness marker before any
  downgrade), binaries in `/usr/local/bin` are symlinks into the active version, `update.yaml` records
  `active_version` and whether it was set by rollback, a lock file prevents re-entrant runs, restart is by
  `SIGHUP` on upgrade and stop/start on downgrade, "If the new version of Teleport fails to start, the
  installation of Teleport is reverted", only current + last working versions are kept, and the updater
  re-execs itself at the active version (https://raw.githubusercontent.com/gravitational/teleport/master/rfd/0184-agent-auto-updates.md).
- **gokrazy** updates whole A/B root partitions with `/update/testboot` then `/update/switch` after a
  successful boot (https://github.com/gokrazy/updater).
- **Erlang** keeps `current` and `old` versions of a module; loading a third "purges the old code and any
  processes lingering in it are terminated" (https://www.erlang.org/doc/system/code_loading.html) — the same
  two-generation rule as `.oldbin`/`app.old`.

### 3.7 Self-update libraries (file replacement)

**inconshreveable/go-update** `Apply()` (https://raw.githubusercontent.com/inconshreveable/go-update/master/apply.go):
1) optional bsdiff patch (`Patcher`), 2) checksum of the *resulting* bytes (`Checksum`, SHA-256 default),
3) signature over that checksum (`Signature`+`PublicKey`, ECDSA default `Verifier`), 4) write
`/path/to/.target.new` with `TargetMode` (0755), 5) `rename(target, .target.old)` (or `OldSavePath`),
6) `rename(.target.new, target)`, 7) delete `.old` (Windows: hide), 8) on failure of 6 try
`rename(.old, target)`; `RollbackError(err)` reports if that also failed and "the file system is left in an
inconsistent state". Note the window between 5 and 6 with no file at `target`, and that nothing is
`fsync`ed. **minio/selfupdate** is the maintained fork with a minisign Ed25519 `Verifier`
(`LoadFromURL/LoadFromFile/Verify`) and a two-phase `PrepareAndCheckBinary` / `CommitBinary`
(https://pkg.go.dev/github.com/minio/selfupdate). **rhysd/go-github-selfupdate** and its maintained fork
**creativeprojects/go-selfupdate** add release discovery (GitHub/GitLab/Gitea/HTTP), semver tags,
`.sha256`/`checksums.txt` validation and ECDSA `.sig`, and delegate the swap to go-update/minio.

## 4. Draining

- **HTTP/1.1**: "A server that sends a 'close' connection option MUST initiate closure of the connection
  … after it sends the response containing the 'close' connection option" and "servers typically close a
  connection in stages. First, the server performs a half-close by closing only the write side … then
  continues to read from the connection until it receives a corresponding close by the client" to avoid
  the RST that "might erase the client's unacknowledged input buffers" (RFC 9112 §9.6,
  https://www.rfc-editor.org/rfc/rfc9112.txt).
- **HTTP/2**: "A server that is attempting to gracefully shut down a connection SHOULD send an initial
  GOAWAY frame with the last stream identifier set to 2^31-1 and a NO_ERROR code … After allowing time for
  any in-flight stream creation (at least one round-trip time), the server MAY send another GOAWAY frame
  with an updated last stream identifier." Streams above last-stream-id "can be safely retried using a new
  connection" (RFC 9113 §6.8, https://www.rfc-editor.org/rfc/rfc9113.txt).
- **Go `net/http`** (https://raw.githubusercontent.com/golang/go/master/src/net/http/server.go): "Shutdown
  works by first closing all open listeners, then closing all idle connections, and then waiting
  indefinitely for connections to return to idle and then shut down. … Shutdown does not attempt to close
  nor wait for hijacked connections such as WebSockets. The caller of Shutdown should separately notify
  such long-lived connections of shutdown and wait for them to close … Once Shutdown has been called on a
  server, it may not be reused". It polls with backoff up to `shutdownPollIntervalMax = 500ms`; keep-alives
  are disabled so h1 responses carry `Connection: close`; `RegisterOnShutdown` callbacks "should start
  protocol-specific graceful shutdown, but should not wait". x/net/http2 registers
  `startGracefulShutdown` there ("sends GOAWAY with ErrCodeNo … The connection isn't closed until all
  current streams are done"; `goAwayTimeout = 1s` applies to error GOAWAYs)
  (https://raw.githubusercontent.com/golang/net/master/http2/server.go). It sends one GOAWAY with the
  highest seen client stream id, not the RFC's two-step 2^31-1 form *(inference from `writeGoAway`)*, so a
  request racing the GOAWAY can be rejected — that is tableflip #12.
- **gRPC-Go**: `GracefulStop` "stops the server from accepting new connections and RPCs and blocks until
  all the pending RPCs are finished"; `Stop` "cancels all active RPCs"
  (https://raw.githubusercontent.com/grpc/grpc-go/master/server.go). Streaming RPCs never "finish" on their
  own: bound it with a timer then `Stop()`.
- **Envoy** drains on hot restart, `/drain_listeners?graceful`, `/healthcheck/fail`, LDS changes; HTTP/1
  gets `Connection: close`, HTTP/2 gets GOAWAY; `--drain-time-s` ramps the fraction told to leave
  (https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/draining). Failing the LB
  health check *first* is the standard way to stop new traffic before the process stops accepting.
- **`SO_LINGER`** (`socket(7)`): with `l_onoff=1`, `close()` blocks until data is sent or `l_linger`
  expires; "When the socket is closed as part of exit(2), it always lingers in the background". `l_linger=0`
  makes close abortive (RST) — never use it to "free the port faster" during a drain; unsent responses are
  lost. Port reuse is not an issue when the socket is inherited anyway.
- **Ordering**: start the new process, let it `accept()` on the inherited socket *before* the old stops.
  Both accepting from the same socket is safe: one accept queue, each connection dequeued once. The old
  process then closes its listener dup (does not affect the socket while the child holds it), runs
  `Shutdown`, and exits within a bound. With separate REUSEPORT sockets the same overlap is where the old
  queue is orphaned on close (unless `tcp_migrate_req`). Envoy's `--parent-shutdown-time-s` and HAProxy's
  `hard-stop-after` exist because drains do not always finish; tableflip has no bound on the old process at
  all (application's job).

## 5. Rollback / abort semantics

- **Health = Ready + probe.** tableflip's `Ready()` is self-reported by the child after it has started
  serving; it says "initialised", not "healthy". Nomad/Teleport add an external probe with a minimum
  healthy time and a deadline. go-binsync should require both: child readiness (pipe byte or `sd_notify`
  style) *and* N consecutive successful probes over `min_healthy_time`, all inside `healthy_deadline`.
- **Abort while old still runs** (child failed to exec, crashed pre-Ready, timed out, or failed probation):
  kill the child (tableflip: SIGKILL; better: SIGTERM, grace, SIGKILL because the child may already have
  accepted connections), restore the file, keep the old process; nothing was lost except connections the
  child had accepted. nginx and HAProxy both keep the old generation's sockets bound for exactly this.
- **Old already exited** (child was Ready, old drained and quit, then child crashes): there is no process
  to fall back to; the supervisor (`Restart=`) re-execs whatever is at the path. If the swap has been
  committed that is the new binary → crash loop → `StartLimitBurst` (5/10 s) → unit failed. Mitigation
  *(design)*: keep an `upgrade.pending` marker until probation passes; an `ExecStartPre=` (or the binary's
  own startup) that finds the marker and a crash counter above threshold renames `app.old` back before
  exec. Teleport's updater does the equivalent ("reverted" if the new version fails to start). Exit codes:
  reserve one (e.g. 75 `EX_TEMPFAIL`-style) for "refused to run, rolled back" so `ExecStopPost` hooks can
  read `$EXIT_STATUS`.
- **Two generations**: `.old` is overwritten each time (go-update, nginx `.oldbin`, Erlang old/current).
  Teleport keeps "the current version and last working version" and marks completeness with a `sha256`
  file. go-binsync should refuse to overwrite `app.old` until the new build has passed probation.
- **ETXTBSY** (https://lwn.net/Articles/866493/): `execve` takes a deny-write reference on the inode
  (`i_writecount < 0`), so `open(O_WRONLY)` on a running binary fails; this survived the 5.15 removal of
  `MAP_DENYWRITE` ("retains the ETXTBSY behavior for the main executable file"). `rename(2)` replaces the
  directory entry, so it always works; the running process keeps the old inode and `readlink
  /proc/self/exe` returns `…/app (deleted)`. Go's `os.Executable()` strips " (deleted)"
  (https://go.dev/src/os/executable_procfs.go), so it silently returns the path of the *new* file. Teleport
  therefore re-execs *itself* via `/proc/self/exe` ("will execute the currently running binary no matter
  what happened to any paths") while graceful restarts keep using `exec.LookPath(os.Args[0])` so the new
  version loads (https://github.com/gravitational/teleport/pull/11283). Rule: upgrade re-exec by path;
  self re-exec by `/proc/self/exe`; never mix.
- **ETXTBSY race in Go** (https://github.com/golang/go/issues/22315, open): a write fd with `O_CLOEXEC`
  "can leak into the forked child of a second thread … until that child calls exec", so a process that
  writes a binary and then execs it while other goroutines fork gets flaky ETXTBSY. Retry with backoff
  (Go's own toolchain did this for years), or write and exec in different processes (go-binsync's transfer
  step vs. the app's re-exec naturally separates them).
- **Atomic install** (https://man7.org/linux/man-pages/man2/fsync.2.html): "Calling fsync() does not
  necessarily ensure that the entry in the directory containing the file has also reached disk. For that
  an explicit fsync() on a file descriptor for the directory is also needed." Sequence: write temp in the
  same directory (same filesystem, or `rename` fails EXDEV — overseer's `mv` shellout is a symptom),
  `fsync(file)`, `chmod 0755`, `link(app, app.old)`, `rename(app.new, app)`, `fsync(dir)`. Hardlink-then-
  rename keeps a valid `app` at every instant; go-update's rename-rename does not.
- **memfd + `execveat`**: `memfd_create` (3.17) gives an anonymous file that can be sealed
  (`F_SEAL_WRITE/SHRINK/GROW/SEAL`); `execveat(fd, "", …, AT_EMPTY_PATH)` (3.19) execs it
  (https://man7.org/linux/man-pages/man2/memfd_create.2.html,
  https://man7.org/linux/man-pages/man2/execveat.2.html). Since 6.3 `MFD_EXEC`/`MFD_NOEXEC_SEAL` and
  `vm.memfd_noexec` (1 = default no-exec, 2 = executable memfds refused) exist and distros warn on
  unflagged calls (https://docs.kernel.org/userspace-api/mfd_noexec.html, https://lwn.net/Articles/918106/).
  Go-specific: `/proc/self/fd/N` as `argv[0]` is clobbered by `ExtraFiles` renumbering unless N >
  3+len(ExtraFiles) (https://github.com/golang/go/issues/66654). Value: verify-then-exec with no TOCTOU and
  no disk write; cost: opaque `/proc/<pid>/exe`, hardened hosts refuse it, and the next upgrade still needs
  a path. Recommendation: disk + rename by default; memfd behind a flag.

## 6. Hooks and lifecycle contracts elsewhere

| System | Hook points | Contract |
|---|---|---|
| systemd | `ExecCondition` (exit 1–254 = skip, 255 = fail), `ExecStartPre/Post`, `ExecReload` (must be synchronous or `Type=notify-reload`), `ExecStopPost` | env `$SERVICE_RESULT`, `$EXIT_CODE`, `$EXIT_STATUS`; `Restart=` matrix; `WatchdogSec` (https://man7.org/linux/man-pages/man5/systemd.service.5.html) |
| dpkg | `preinst/postinst/prerm/postrm` with `install/upgrade/configure/remove/abort-upgrade/failed-upgrade …` | idempotent ("if it is run successfully, and then it is called again, it doesn't bomb out"), explicit unwind sequence (`new-postrm abort-upgrade` → `old-postinst abort-upgrade`), documented "point of no return" (https://www.debian.org/doc/debian-policy/ch-maintainerscripts.html) |
| Kubernetes | `postStart` (concurrent with ENTRYPOINT; failure kills container), `preStop` (before TERM; counts against `terminationGracePeriodSeconds`; failure kills) | at-least-once, `exec`/`httpGet`/`sleep`, no parameters passed (https://kubernetes.io/docs/concepts/containers/container-lifecycle-hooks/) |
| Nomad | template `change_mode` `noop/restart/signal/script` with `change_script{command,args,timeout,fail_on_error}`, `splay`; `update{canary,auto_revert,auto_promote,min_healthy_time,healthy_deadline,progress_deadline}` | health-gated promote/revert (https://developer.hashicorp.com/nomad/docs/job-specification/template) |
| Ansible | `notify` → handlers run once at end of play, in definition order, skipped if the play fails unless `force_handlers`; `flush_handlers` | coalesced, ordered (https://docs.ansible.com/ansible/latest/playbook_guide/playbooks_handlers.html) |
| overseer | `PreUpgrade(tempPath)`; binary self-check via `OVERSEER_BIN_CHECK` token | veto before swap |
| nginx/unicorn | none — signals *are* the hooks (`USR2/WINCH/QUIT/HUP/TERM`) | operator-driven rollback |

Minimal set that covers them: `pre-fetch`, `pre-swap` (staged file verified, can veto), `pre-exec`,
`on-ready`, `on-healthy` (commit), `on-abort` (child failed, old continues), `on-rollback` (file restored),
`post-upgrade` (old exited). Pre-hooks fail closed; post-hooks log unless `fail_on_error`; every hook has
a timeout and is idempotent/at-least-once.

## 7. Security and integrity

TUF's threat list (https://theupdateframework.github.io/specification/latest/): arbitrary installation,
endless data, extraneous dependencies, fast-forward, indefinite freeze, mix-and-match, rollback, wrong
software. Its mechanisms: signed roles (root/targets/snapshot/timestamp), expiry on every metadata file,
version numbers that clients refuse to see decrease, hashes+lengths for targets. A single-binary tool does
not need four roles, but it needs the *properties*:

1. **Signed manifest**, not just a signed blob: `{name, version, sha256, size, created, expires,
   min_client_version?}` signed with Ed25519. minisign's trusted comment is signed and is documented for
   exactly this: "the intended file name, timestamps, resource identifiers, or version numbers to prevent
   downgrade attacks" (https://jedisct1.github.io/minisign/). cosign `sign-blob --bundle` adds a Rekor
   inclusion proof and keyless identity if that fits the CI (https://docs.sigstore.dev/cosign/signing/signing_with_blobs/).
2. **Rollback**: refuse `version <= installed` unless an explicit `--allow-downgrade` (Teleport's
   downgrade path validates a DB backup first and refuses otherwise). **Freeze**: reject expired manifests;
   record `created` and refuse older-than-last-seen. **Endless data**: enforce `size` while reading.
3. **Verify the bytes you exec**: hash the staged file after `fsync`, from an fd you keep open, in a
   directory writable only by the upgrade user; or memfd+seals to remove the TOCTOU entirely.
4. **Delta transfers**: go-update verifies the checksum of the *patched result*, not the patch — go-binsync's
   sync step should be treated the same way: whatever the transfer did, the staged file must hash to the
   manifest's `sha256` before it is linked into place.
5. Public key pinned in the go-binsync agent; rotation via a signed "root" list is TUF's job if ever needed.

## 8. Failure-mode catalogue

| Failure | tableflip | nginx | HAProxy (master-worker) | Envoy | go-binsync should |
|---|---|---|---|---|---|
| New binary won't exec (ENOENT/EACCES/ETXTBSY) | `can't start process` error, old continues | `execve() failed`, pid file renamed back | master re-exec fails; old workers keep running | new process never registers; parent keeps serving | abort, restore `app.old`, retry ETXTBSY with backoff |
| New process crashes before Ready | `child %s exited`, old continues | new master dies; old master drops `.oldbin`, respawns workers if needed | new fails to bind → old workers "restart listening" | parent unaffected until sockets are handed over | abort as above; `on-abort` hook with exit status |
| New process hangs before Ready | `UpgradeTimeout` (60 s) → SIGKILL | operator (no timeout) | operator | supervisor script has no readiness notion | `ready_timeout` → SIGTERM, grace, SIGKILL |
| New process Ready then unhealthy | not detected — old already exiting | operator `HUP` old + `QUIT` new | operator | drain already started; old dies after `--parent-shutdown-time-s` | probation window before old drains; abort restores file and kills child |
| New process crashes after old exited | nothing left; systemd `Restart=` loops on new binary | `.oldbin` gone; manual | master respawns workers of current config | `hot-restarter.py` kills all and exits 1 | `upgrade.pending` marker + startup/`ExecStartPre` self-revert; bounded by `StartLimitBurst` |
| Old process never finishes draining | app's problem; blocks next upgrade (#52) | `WINCH` workers linger until requests end | `hard-stop-after` | `--parent-shutdown-time-s` (900 s) | `drain_timeout` then `Server.Close()`; long-lived conns closed via `RegisterOnShutdown` |
| Both accepting concurrently | same socket: safe | same socket: safe | same socket via SCM_RIGHTS | same socket via SCM_RIGHTS | inherit; never REUSEPORT unless ≥5.14 + `tcp_migrate_req=1` |
| Second upgrade while one in flight | `upgrade in progress` / `parent hasn't exited` | `USR2` ignored if `ngx_new_binary` set *(inference from `ngx_change_binary` flag)* | `mworker-max-reloads` caps generations | epoch mismatch → refuse | single in-flight; queue or reject with explicit error |
| Supervisor loses track of main PID | `PIDFile` rewritten at `Ready()`; needs `PIDFile=` in unit | pid file swapped | `-Ws` notifies systemd | shared memory + supervisor script | `sd_notify MAINPID=` when child ready; or run under systemd fd store where PID does not change meaning |
| Passed fd becomes blocking | fixed by custom `StartProcess` (#60) | n/a (C) | n/a | n/a | set `O_NONBLOCK` on dups before `net.FileListener` |
| Unix socket path unlinked by wrong generation | only `Stop()` before success unlinks | n/a | n/a | n/a | same rule: unlink only on final shutdown |
| Disk full / crash mid-install | go-update: `.new` orphan or `RollbackError` | n/a | n/a | n/a | temp+fsync+link+rename+fsync(dir); verify `sha256` marker before trusting a staged file |
| Bad signature / downgrade / expired manifest | n/a | n/a | n/a | n/a | refuse before any file is touched |
| Hook fails | n/a | n/a | n/a | n/a | pre-* abort; post-* log unless `fail_on_error` |

## 9. Implications for go-binsync

**Choose the holder of the sockets.** Three viable modes, in order of preference:

1. **In-app (tableflip-style) library** for Go services that link it: exec by path, fds by inheritance,
   readiness over a pipe. Full overlap, per-connection zero loss, rollback while the old process is still
   serving. go-binsync's agent triggers it (signal or control socket) and runs the probe.
2. **systemd fd store mode** for services that only add `sd_notify`/`sd_listen_fds`: go-binsync swaps the
   file and runs `systemctl restart`; kernel backlog covers the gap; rollback = restore file + restart.
   Simpler, no overlap, and crash-looping is bounded by systemd not by go-binsync.
3. **External socket-holder (overseer/HAProxy master style)**: only if the app cannot be modified at all.
   It puts go-binsync in the data path and makes go-binsync itself the thing that cannot be upgraded live.

**Lifecycle state machine (mode 1):**

```
IDLE
 └─ sync ──────────────► STAGED        app.new written, fsync'd, sha256+signature+version+expiry verified; hook pre-swap
     (hook pre-fetch)      │
                           ├─ link(app, app.old); rename(app.new, app); fsync(dir); write upgrade.pending
                           ▼
                        INSTALLED      hook pre-exec
                           │  signal/RPC old process → it execs `app` (path, not /proc/self/exe) with fds
                           ▼
                        STARTING       wait ready byte  ── ready_timeout ─► ABORTING
                           │ (child crash/exit → ABORTING)
                           ▼  hook on-ready
                        PROBING        HTTP/exec probe on new pid, min_healthy_time within healthy_deadline
                           │  fail/deadline ─────────────────────────────► ABORTING
                           ▼  hook on-healthy
                        COMMITTED      old: close listener dups, Shutdown(drain_timeout) then Close(), exit;
                           │           remove upgrade.pending; keep app.old (one generation)
                           ▼  hook post-upgrade (when old has exited)
                        IDLE

ABORTING: SIGTERM child, grace, SIGKILL; rename(app.old, app); fsync(dir); remove upgrade.pending;
          hook on-rollback (BINSYNC_REASON, BINSYNC_EXIT_STATUS); old process continues unchanged → IDLE
```

Invariants (borrowed from tableflip and nginx): one upgrade in flight; the old process keeps its listening
sockets until COMMITTED; no old code after COMMITTED + drain; `app.old` is only overwritten in STAGED of the
*next* upgrade, never during this one; `app` is a valid executable at every instant (hardlink+rename).

**Startup self-check (covers "old already exited"):** on start, if `upgrade.pending` exists and this pid
is not the child of an in-flight upgrade (env var absent), increment a crash counter in the marker; above
N, rename `app.old` back and exit with the reserved code so `Restart=` starts the old build. This is the
only defence once the previous process is gone; it is also what Teleport and Nomad `auto_revert` amount to.

**Hook contract:** hooks are executables under a directory, run with a per-hook timeout, env:
`BINSYNC_EVENT` (`pre-fetch|pre-swap|pre-exec|on-ready|on-healthy|on-abort|on-rollback|post-upgrade`),
`BINSYNC_APP_PATH`, `BINSYNC_STAGED_PATH`, `BINSYNC_OLD_VERSION`, `BINSYNC_NEW_VERSION`,
`BINSYNC_OLD_PID`, `BINSYNC_NEW_PID`, `BINSYNC_REASON`, `BINSYNC_EXIT_STATUS`, `BINSYNC_ATTEMPT`. Non-zero
from a `pre-*` hook aborts (dpkg/k8s semantics); from others it is logged unless configured
`fail_on_error` (Nomad). Hooks may run more than once (k8s at-least-once) and must be idempotent (dpkg).

**Draining defaults:** `ready_timeout` 60 s (tableflip's default), `min_healthy_time` 10 s /
`healthy_deadline` 5 m (Nomad's), `drain_timeout` 30 s then `Close()`; document that WebSocket/SSE/gRPC
stream handlers must hook `RegisterOnShutdown` or they will be cut at `drain_timeout`; send
`Connection: close` / GOAWAY immediately on COMMITTED (Go does this via `Shutdown`).

**Things to refuse up front:** REUSEPORT-based handoff on kernels < 5.14 or with `tcp_migrate_req=0`;
`/proc/self/exe` as the upgrade exec path; overwriting `app` in place (ETXTBSY anyway); cross-filesystem
staging directories (rename would be a copy); unsigned or unversioned manifests.

## 10. References

- Cloudflare, "Graceful upgrades in Go" — https://blog.cloudflare.com/graceful-upgrades-in-go/
- tableflip source — https://github.com/cloudflare/tableflip (upgrader.go, child.go, parent.go, fds.go); docs https://pkg.go.dev/github.com/cloudflare/tableflip; issues #12, #29, #52, #59, #60, #74
- nginx "Controlling nginx" — https://nginx.org/en/docs/control.html; `src/core/nginx.c` (`ngx_exec_new_binary`, `ngx_add_inherited_sockets`)
- HAProxy "Truly Seamless Reloads with HAProxy – No More Hacks!" — https://www.haproxy.com/blog/truly-seamless-reloads-with-haproxy-no-more-hacks; management guide https://docs.haproxy.org/3.0/management.html
- Envoy hot restart — https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/hot_restart; draining — https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/draining; CLI — https://www.envoyproxy.io/docs/envoy/latest/operations/cli; `restarter/hot-restarter.py`
- LWN, "The SO_REUSEPORT socket option" — https://lwn.net/Articles/542629/; "Socket migration for SO_REUSEPORT" — https://lwn.net/Articles/837506/, https://lwn.net/Articles/853637/
- Kernel `ip-sysctl` (`tcp_migrate_req`) — https://www.kernel.org/doc/html/latest/networking/ip-sysctl.html; commit f9ac779f881c (v5.14); https://kernelnewbies.org/Linux_5.14
- man pages: unix(7), socket(7), fsync(2), memfd_create(2), execveat(2), pidfd_open(2) — https://man7.org/linux/man-pages/
- systemd: sd_listen_fds(3), sd_pid_notify_with_fds(3)/sd_notify(3), systemd.service(5), systemd.unit(5) via man7.org; https://systemd.io/FILE_DESCRIPTOR_STORE/
- LWN, "The shrinking role of ETXTBSY" — https://lwn.net/Articles/866493/; Go `os/executable_procfs.go` — https://go.dev/src/os/executable_procfs.go; golang/go#22315, #66654; Teleport PR #11283 — https://github.com/gravitational/teleport/pull/11283
- memfd noexec — https://docs.kernel.org/userspace-api/mfd_noexec.html, https://lwn.net/Articles/918106/
- Go `net/http/server.go` (Shutdown, RegisterOnShutdown), `net/file.go`, `os/exec/exec.go`; x/net `http2/server.go`; grpc-go `server.go`
- RFC 9112 §9.6 — https://www.rfc-editor.org/rfc/rfc9112.txt; RFC 9113 §6.8 — https://www.rfc-editor.org/rfc/rfc9113.txt
- inconshreveable/go-update `apply.go` — https://github.com/inconshreveable/go-update; minio/selfupdate — https://pkg.go.dev/github.com/minio/selfupdate; creativeprojects/go-selfupdate; rhysd/go-github-selfupdate
- jpillora/overseer — https://github.com/jpillora/overseer; facebookarchive/grace — https://github.com/facebookarchive/grace
- Tailscale `clientupdate` — https://github.com/tailscale/tailscale/blob/main/clientupdate/clientupdate.go; Teleport RFD 184 — https://github.com/gravitational/teleport/blob/master/rfd/0184-agent-auto-updates.md
- unicorn SIGNALS — https://yhbt.net/unicorn/SIGNALS.html; puma restart — https://github.com/puma/puma/blob/master/docs/restart.md; Erlang code loading — https://www.erlang.org/doc/system/code_loading.html; gokrazy updater — https://github.com/gokrazy/updater
- Kubernetes lifecycle hooks — https://kubernetes.io/docs/concepts/containers/container-lifecycle-hooks/; Nomad update/template — https://developer.hashicorp.com/nomad/docs/job-specification/update, https://developer.hashicorp.com/nomad/docs/job-specification/template; Debian policy ch. 6 — https://www.debian.org/doc/debian-policy/ch-maintainerscripts.html; Ansible handlers — https://docs.ansible.com/ansible/latest/playbook_guide/playbooks_handlers.html
- TUF specification — https://theupdateframework.github.io/specification/latest/; minisign — https://jedisct1.github.io/minisign/; cosign blobs — https://docs.sigstore.dev/cosign/signing/signing_with_blobs/
