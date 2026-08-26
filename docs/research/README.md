# Research index

Background research and measurements behind `docs/DESIGN.md`. Each document
has a summary of findings at the top, cites primary sources inline, and marks
inferences as such. All measurements were taken on 2026-08-26 with Go 1.26.4 on
linux/amd64.

| Document | Question | Headline result |
|---|---|---|
| [benchmark-local.md](benchmark-local.md) | Empirical: how does a 29.6 MB Go web server change on a one-line edit, and what do the delta/chunk tools cost? Reproducible via `bench/run.sh`. | One added statement: 13.4 % of bytes in 965 K runs; bsdiff 60 KB / hdiffz 75 KB / zstd `--patch-from` 213 KB / xdelta3 344 KB vs 8.4 MB full; desync re-sends 92 % of the full download; unstripped DWARF makes every patch ~8.5 MB. |
| [go-binary-layout.md](go-binary-layout.md) | How does cmd/link lay out a binary, and what exactly ripples when one function grows or one string gets longer? | 32-byte function alignment; `.text` in import post-order; `.rodata` sorted by size; pclntab offset-based since Go 1.18; PIE makes deltas larger; `/proc/self/exe` runs the old inode after a rename. |
| [binary-delta.md](binary-delta.md) | Which delta encoders and executable-aware patchers exist, and what do published benchmarks say? | bsdiff's approximate matching is the right primitive; HDiffPatch is the strongest C tool; Zucchini buys only ~10 % on ELF; pure-Go options are thin (klauspost raw-dict zstd, go-bsdiff). |
| [cdc-cas.md](cdc-cas.md) | Can content-defined chunking + a CAS store do the job? | No for shifted binaries: 49–70 % of chunks invalidated on a VictoriaMetrics one-line change; delta encoding wins ~100×. CDC remains a storage/cold-start substrate. |
| [update-systems.md](update-systems.md) | How do Chrome, Firefox, Windows, Steam, ostree, Android, OCI, Riot, wharf handle release metadata, skipped versions, transport and fan-out? | One conditional GET on a signed pointer; content-addressed immutable objects; S3/GCS have no multi-range; UUP vs K-matrix vs chain trade-offs. |
| [zero-downtime-upgrade.md](zero-downtime-upgrade.md) | How do tableflip, nginx, HAProxy, Envoy, systemd hand off sockets, drain, and roll back? | Same-socket inheritance is the only loss-free handoff everywhere; `SO_REUSEPORT` drops connections below 5.14; hardlink+rename install; `upgrade.pending` marker for post-exit crashes. |

Scratch artifacts from the agents' own experiments (VictoriaMetrics builds,
chunk dumps) were not kept; the `bench/` harness reproduces the equivalent
measurements on the `bench/testsrv` corpus.
