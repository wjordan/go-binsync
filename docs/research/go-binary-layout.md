# Go binary layout, build determinism, and what a one-line change does to the ELF

Research notes for binsync. Scope: Go-compiled linux/amd64 web-server binaries
(30–100 MB), internal linking, updated over a high-latency link where the typical
release delta is one line of code or one text constant.

Two kinds of sources are used: (1) primary sources — the Go linker/runtime source
(the installed Go 1.26.4 at `/usr/local/go/src`, plus `master` on GitHub), Go
release notes, issues and CLs, Linux man pages; (2) a controlled experiment run
for this document (Section 0). Numbers in this document come from that
experiment unless a citation says otherwise. Where I extrapolate, I say so.

## Summary of findings

1. **Compressed DWARF is ~95 % of the delta.** For every one-line change we
   tried, `.debug_*` (zlib-compressed by default, `SHF_COMPRESSED` since Go 1.19)
   contributed 5.0–5.2 MB of a 5.1–5.6 MB patch on a 25.3 MB binary. With
   `-ldflags="-s -w"` the same one-line change patches in 119 KB (bsdiff),
   332 KB (zstd `--patch-from`), 642 KB (xdelta3). Stripping is the single
   decision that matters; everything else is second order.
2. **Functions are 32-byte aligned on amd64** (`funcAlign = 32`,
   `cmd/link/internal/amd64/l.go`; all 15,389 of 15,390 text symbols in the test
   binary sit on 32-byte boundaries). A function that grows by ≤ its padding
   moves nothing: our "+7 bytes" edit shifted zero symbols and changed 750
   bytes of `.text`. A "+47 bytes" edit shifted the following 7,281 functions
   by +32.
3. **`.text` order is deterministic and package-major**: runtime-dependency
   packages first, then all other packages in import post-order (dependencies
   before importers, `main` last), FIPS symbols gathered at the very end; inside
   a package, object-file definition order. So a change in a leaf library shifts
   roughly everything "above" it in the import graph; a change in `main` shifts
   only `main`'s tail and the FIPS block. (`cmd/link/internal/ld/lib.go`
   `postorder`, `loader.AssignTextSymbolOrder`, `ld/data.go` `textaddress`.)
4. **A shift of +32 does not just move bytes; it rewrites references.** 9,410
   *unmoved* functions changed because their `CALL rel32` / RIP-relative
   operands point across the shift boundary: 128,110 differing bytes in 114,519
   runs of average length 1.12 bytes. This is what a generic delta encoder pays
   ~250 KB (zstd) for and what a reference-aware encoder (Courgette/Zucchini
   style) would encode in almost nothing.
5. **`.rodata`/`.data` symbols are sorted by size, then by symbol index**
   (`dodataSect`). Adding a 15-byte string literal anywhere inserts it into the
   16-byte size class and pushes every larger read-only symbol (1,421 symbols,
   +16/+32) — which in turn rewrites RIP-relative operands in code that
   references them. Same-length constant edits move nothing (105 differing
   bytes total; 452-byte xdelta3 patch).
6. **pclntab is offset-based, not address-based, since Go 1.18** (`functab`
   entries and `_func.entryOff` are offsets from `runtime.text`; CL 352191 cut
   relocations 18.3 %). Go 1.26 made `.gopclntab` relocation-free, moved
   `go:func.*` and `findfunctab` into it, dropped `.gosymtab`, and put
   `moduledata` in its own `.go.module` section. Residual churn is small and
   structural: a changed function's pc-tables shift the shared `pctab`, which
   changes the `uint32` offsets in every later `_func` (≈50–70 KB zstd patch).
7. **Non-PIE `.rodata` still contains absolute addresses** — every type
   descriptor has `Equal` and `GCData` pointers, every itab has `Inter`, `Type`
   and `Fun[]` code pointers — but they only change when the *target* moves,
   and they are a small share of the patch (`.rodata`: 12 KB zstd).
8. **PIE makes deltas larger, not more localized.** The 66,170
   `R_X86_64_RELATIVE` entries (1.59 MB `.rela`) contain absolute slot and target
   addresses; PIE stripped patch = 397 KB zstd / 131 KB bsdiff vs 332 KB / 119 KB
   for the non-PIE build, and the binary is 6.3 % larger. Linux stays
   `-buildmode=exe` by default (`internal/platform.DefaultPIE` returns true only
   for android, ios, windows, darwin).
9. **Builds are bit-for-bit reproducible locally** (two builds of the same tree
   were identical, with and without `-buildid=`). Go 1.21+ claims "perfectly
   reproducible" toolchains; user binaries are reproducible given the same
   toolchain version, `GOOS/GOARCH/GOAMD64`, `CGO_ENABLED=0`, `-trimpath`, the
   same module graph and VCS state. The one known trap is the build ID, whose
   action-ID half hashes the working directory even with `-trimpath`
   (#34186 → #33772, #76743); fix: `-ldflags=-buildid=`.
10. **Undocumented / new linker knobs**: `-randlayout=N` (Go 1.23+, shuffles
    function order with a seed; exists to fuzz layout dependence, not to help
    us); `-funcalign=N` (Go 1.25+; `=64` absorbed our +47-byte growth at +1.4 %
    binary size — probabilistic, not a strategy); `-compressdwarf=false`
    (binary +34 %, but DWARF becomes delta-friendly: patch 700 KB zstd instead
    of 5.6 MB). There is no exposed function/section padding knob.
11. **PGO is delta-hostile by construction**: it changes inlining and
    devirtualization decisions wherever the profile changed, so a new
    `default.pgo` per release re-shapes many functions. Edits to small
    *inlinable* functions have the same fan-out effect: our edit to the inlined
    `logger.Infof` changed 5,318 callers.
12. **Replacing a running binary**: `open(2)` for write on a running executable
    fails with `ETXTBSY`; `rename(2)` over it succeeds; afterwards
    `/proc/PID/exe` reads `... (deleted)` and **executing `/proc/self/exe` runs
    the old inode**, while executing the path runs the new file.
    `os.Executable()` strips the `" (deleted)"` suffix and returns the path
    (which now names the new binary). Verified empirically; see Section 5.
13. **`debug/gosym` works on stripped Go 1.26 binaries**: `.gopclntab` +
    `.text` address is enough to recover all 15,388 function boundaries and
    file:line — useful for a function-aware differ. `.symtab` is not needed.

## 0. Experiment used throughout

Target: `github.com/VictoriaMetrics/VictoriaMetrics/app/victoria-metrics` (a
representative large Go server), built with the installed toolchain:

```
go1.26.4 linux/amd64, CGO_ENABLED=0, go build -trimpath [variants]
```

Baseline `A` = 25,318,603 bytes. Variants were produced with `go build -overlay`
so the checked-out tree was never modified:

| variant | edit | effect on layout |
|---|---|---|
| `strsame` | one char changed in a flag-description string in `main.go` | same length; nothing moves |
| `imm` | `cgroup.SetGOGC(30)` → `31` in `main.main` | immediate operand only |
| `strgrow` | +6 bytes in that flag-description string | string moves within size-sorted `.rodata`; 19 functions' RIP-rel operands change; no function moves |
| `libcode3` | `if maxArgLen < 0 { maxArgLen = -maxArgLen }` added to `lib/logger.formatLogMessage` (773 → 780 B) | growth absorbed by 32-byte padding; nothing moves |
| `libcode2` | `if len(format) > 1<<20 { panic("format too long") }` added to the same function (773 → 820 B) | crosses padding: 7,281 later functions shift +32; new 15-byte string reorders `.rodata` |
| `maincode` | one `if … { logger.Fatalf(...) }` added to `main.main` | 672 functions shift +32 (rest of `main` + FIPS block); new string reorders `.rodata` |
| `libcode` | one `if` added to `lib/logger.Infof`, which is **inlined** everywhere | 5,414 functions moved, 5,318 more changed — the fan-out case |

Measurements: `readelf`, `go tool nm`, per-section byte comparison at identical
offsets, per-section `zstd -19 --long=27 --patch-from`, `xdelta3 -9`, `bsdiff`.

Headline results (`A → B`, whole-file patch sizes):

| pair | flags | bytes differing at same offset | xdelta3 | zstd patch | bsdiff |
|---|---|---|---|---|---|
| strsame | default | 105 (0.00 %) | 452 B | 3.1 KB | — |
| imm | default | 106 | 483 B | 3.1 KB | — |
| strgrow | default | 18,711 (0.07 %) | 896 B | 3.3 KB | — |
| libcode3 | default | 9.39 MB (37 %) | 5.15 MB | 5.10 MB | — |
| libcode2 | default | 16.3 MB (64.5 %) | 6.02 MB | 5.65 MB | 5.52 MB |
| maincode | default | 9.47 MB (37 %) | 5.33 MB | 5.23 MB | 5.26 MB |
| libcode2 | `-s -w` (17.9 MB) | 9.23 MB (51.6 %) | 642 KB | 332 KB | **119 KB** |
| libcode | `-s -w` | 5.1 MB | 481 KB | 246 KB | — |
| libcode2 | `-s -w -funcalign=64` (18.1 MB) | 5.6 MB | 323 KB | 202 KB | — |
| libcode2 | `-compressdwarf=false` (33.9 MB) | 23.6 MB | 1.29 MB | 700 KB | — |
| libcode2 | `-buildmode=pie` (26.9 MB) | 17.7 MB | 6.25 MB | 5.84 MB | — |
| libcode2 | `-buildmode=pie -s -w` (19.5 MB) | 10.7 MB | 950 KB | 397 KB | 131 KB |

Per-section attribution of the zstd patch for `libcode2` (default flags):
`.debug_info` 2.13 MB, `.debug_loclists` 1.31 MB, `.debug_line` 1.28 MB,
`.debug_rnglists` 413 KB, `.text` 248 KB, `.debug_frame` 134 KB, `.gopclntab`
71 KB, `.debug_addr` 30 KB, `.rodata` 12 KB, `.symtab` 7 KB, `.data` 1.6 KB,
`.itablink` 769 B, everything else < 400 B. Sum 5.64 MB.

Caveat: one program, one toolchain, one architecture. Ratios should transfer;
absolute numbers will not.

## 1. ELF layout produced by cmd/link

### 1.1 Sections and sizes (baseline A, Go 1.26.4, internal linking, non-PIE)

| section | bytes | % of file | contents / notes |
|---|---|---|---|
| `.note.go.buildid` | 100 | — | Go build ID note (`-ldflags=-buildid=` empties it) |
| `.note.gnu.build-id` | 36 | — | `NT_GNU_BUILD_ID`, emitted by default since Go 1.24 and derived from the Go build ID; `-B none` disables (https://go.dev/doc/go1.24#linker) |
| `.text` | 7,879,121 | 31.1 % | code; `runtime.text` … `runtime.etext` |
| `.rodata` | 3,504,025 | 13.8 % | strings, type descriptors, itabs, static composite literals, `go:string.*` |
| `.gopclntab` | 5,900,186 | 23.3 % | pcHeader, funcnametab, cutab, filetab, pctab, functab+`_func`, `go:func.*` (funcdata), findfunctab |
| `.typelink` | 18,012 | — | `int32` offsets of types from `runtime.types` |
| `.itablink` | 5,520 | — | `*itab` pointers (absolute) |
| `.go.buildinfo` | 1,248 | — | `debug/buildinfo` blob (`"\xff Go buildinf:"`) |
| `.go.fipsinfo` | 120 | — | FIPS 140-3 module self-check info (`sym.SFIPSINFO`; arrived with the Go Cryptographic Module in Go 1.24 — inferred from the release, not verified in source) |
| `.go.module` | 592 | — | `runtime.firstmoduledata` (Go 1.26+; was in `.noptrdata`) |
| `.noptrdata` | 482,881 | 1.9 % | pointer-free initialized data |
| `.data` | 99,474 | 0.4 % | initialized data with pointers |
| `.bss` / `.noptrbss` | 175,480 / 33,966,656 | 0 (NOBITS) | |
| `.debug_abbrev/line/frame/info/loclists/rnglists/addr` | 5,809,264 total | 22.9 % | DWARF 5 (Go 1.25+), **all `SHF_COMPRESSED`** |
| `.symtab` + `.strtab` | 502,416 + 1,107,111 | 6.4 % | 20,933 × 24-byte ELF symbols |

`-s -w` → 17,899,682 bytes (−29.3 %). `-buildmode=pie` → 26,922,096 (+6.3 %).
`-compressdwarf=false` → 33,927,763 (+34.0 %). These match the published
ratios for other large binaries: `.gopclntab` 23.8 %, `.text` 30.3 %, `-s` saving
34 MB of 109 MB for Istio's pilot-agent
(https://blog.howardjohn.info/posts/go-binary-size/); pclntab 16–17 % and "for
every 6 bytes of code+data, 5 bytes of Go metadata" for CockroachDB
(https://dr-knz.net/go-executable-size-visualization-with-d3-2021.html).

Segments (`readelf -l`): three `LOAD`s at 4 KiB alignment: `R E` text at
0x400000, `R` rodata (rodata + gopclntab + typelink + itablink) at 0xb85000,
`RW` data at 0x1483000. `cmd/link/internal/amd64/obj.go` sets
`FlagRound = 4096`, `FlagTextAddr = Rnd(1<<22, FlagRound) + HEADR` for ELF.
`ld/data.go address()` rounds each segment's virtual address
(`va = uint64(Rnd(int64(va), *FlagRound))`) and file offset
(`seg.Fileoff = Rnd(prev.Fileoff+prev.Filelen, *FlagRound)`) — so growth inside a
segment shifts later segments only when it crosses a 4 KiB boundary (in our
experiments it never did; file offsets of `.rodata` etc. were unchanged).

Go 1.26 release notes (https://go.dev/doc/go1.26#linker) list the layout
changes relevant to tooling: `moduledata` in `.go.module`; `pcHeader` no longer
records text start ("That `pcHeader` change was made so that the `.gopclntab`
section no longer contains any relocations. On platforms that support relro, the
section has moved from the relro segment to the rodata segment"); funcdata and
findfunctab moved from `.rodata` into `.gopclntab`; `.gosymtab` removed; ELF
section headers now sorted by address. Ian Lance Taylor's umbrella issue is
https://github.com/golang/go/issues/76038.

### 1.2 How `.text` is ordered

The order is fixed by three pieces of linker code (quotes from Go 1.26.4 /
master):

* **Library order.** `ld/lib.go loadlib` loads `ctxt.Library` in discovery
  order (`for i := 0; i < len(ctxt.Library); i++ { loadobjfile(ctxt, lib) }`,
  appending imports as objects are read; `main` is first, `runtime` is force-
  loaded), then `ctxt.Library = postorder(ctxt.Library)` — a DFS over
  `lib.Imports` appending each library *after* its imports. Result:
  dependencies precede importers, `main` is last.
* **Internal-first.** `ctxt.Textp = ctxt.loader.AssignTextSymbolOrder(ctxt.Library, intlibs, ctxt.Textp)`
  with `intlibs[i] = isRuntimeDepPkg(lib.Pkg)` →
  `objabi.LookupPkgSpecial(pkg).Runtime` (`cmd/internal/objabi/pkgspecial.go`):
  `runtime`, `internal/runtime/*`, `internal/abi`, `internal/bytealg`,
  `internal/byteorder`, `internal/chacha8rand`, `internal/coverage/rtcov`,
  `internal/cpu`, `internal/goarch`, `internal/godebugs`,
  `internal/goexperiment`, `internal/goos`, `internal/profilerecord`,
  `internal/strconv`, `internal/stringslite`. `AssignTextSymbolOrder`'s doc:
  "populates the Textp slices within each library and compilation unit,
  insuring that packages are laid down in dependency order (internal first,
  then everything else)". Two passes over `libs` (internal, then the rest);
  within a library, symbols are appended in object-file definition order
  (`for i := 0; i < r.NAlldef(); i++`); external (host-object) symbols go
  first. Dupok symbols (generic instantiations, closures shared across
  packages, `type:.eq.*`) are "chosen sort of arbitrarily (the first containing
  package that the linker loads)" and appended after that library's regular
  symbols. No sorting by name or size.
  (https://raw.githubusercontent.com/golang/go/master/src/cmd/link/internal/loader/loader.go)
* **Address assignment.** `ld/data.go textaddress()`: `sect.Align = Funcalign`
  (amd64 `funcAlign = 32`, `cmd/link/internal/amd64/l.go`); if
  `*flagRandLayout != 0` it seeds `rand.NewSource(*flagRandLayout)` and
  `r.Shuffle`s all text symbols after `go:buildid` and any C sub-symbols; then
  unconditionally `sort.SliceStable(ctxt.Textp, by ldr.SymType)` "so that FIPS
  symbols are gathered together, with the FIPS start and end symbols bracketing
  them". `assignAddress` rounds each function to `max(sym align, Funcalign)`.

Within a package the object-file order is the compiler's enqueue order:
`cmd/compile/internal/gc/main.go` iterates `typecheck.Target.Funcs` in order,
`enqueueFunc → prepareFunc → ir.InitLSym` runs *serially* ("Set up the
function's LSym early to avoid data races with the assemblers"), and
`obj.(*Link).InitTextSym` does `ctxt.Text = append(ctxt.Text, s)`. Backend
compilation is parallel (longest functions first) but does not affect emission
order. Compiler-generated wrappers (`noder.MakeWrappers`) and closures are
appended around their parents. I did not audit the object writer for a final
sort; the empirical evidence (bit-identical rebuilds, stable order across our
variants) is consistent with "declaration order".

Observed in A: `internal/abi.BoundsDecode` at 0x401000 is the first function,
`runtime.main` is #910 of 15,390, `main.main` is #14,718, and the last ~670
symbols are `crypto/internal/fips140/...` followed by `go:textfipsend`.

### 1.3 How data sections are ordered

`ld/data.go dodataSect` (Go 1.26.4, lines 2349–2427): for every data kind except
`SPCLNTAB` ("PCLNTAB was built internally, and already has the proper order")
and `SELFGOT`, symbols are sorted by

```go
case isz != jsz: return isz < jsz
...
return si < sj // break ties by symbol number
```

with `runtime.zerobase` placed after the zero-sized symbols. `STYPE` symbols get
special handling (typelinks sorted by type string, then itabs, then types by
size). The section allocation order in `allocateDataSections` (Go 1.26.4) is:
`.go.buildinfo`, `.go.fipsinfo`, `.go.elfsect`, `.go.module`, `.got`,
`.noptrdata`, `.init_array`, `.data`, `.bss`, `.noptrbss`, `.go.fuzzcntrs`,
`.rodata` (all `sym.ReadOnly` kinds, each kind as a contiguous run),
`.gopclntab`, then `.typelink` and `.itablink` (named `.data.rel.ro.typelink`
etc. under relro).

Consequence: **a symbol's position in `.rodata` is a function of its size and
its loader index**, not of its package. Inserting or resizing a constant moves
every symbol of equal-or-larger size after it (Section 2.2).

### 1.4 pclntab (Go 1.20 format, magic `0xfffffff1`)

Layout (`cmd/link/internal/ld/pcln.go`, `runtime/symtab.go`):

```
pcHeader{ magic, pad, minLC, ptrSize, nfunc, nfiles, textStart(unused since 1.26),
          funcnameOffset, cuOffset, filetabOffset, pctabOffset, pclnOffset }
funcnametab   NUL-terminated names            (A: 1,078,072 B)
cutab         per-CU file index → filetab off (69,624 B)
filetab       NUL-terminated file names       (62,888 B)
pctab         deduplicated pc-value tables    (2,147,704 B)
functab       nfunc+1 × {entryoff u32, funcoff u32} + _func structs (1,460,264 B)
go:func.*     funcdata (stack maps, etc.)     (1,043,088 B)   [inside .gopclntab since 1.26]
findfunctab   bucket index for PC lookup      (38,474 B)
```

`functab.entryoff` is "relative to runtime.text"; `_func.entryOff` likewise;
`_func.pcsp/pcfile/pcln/npcdata` are `uint32` offsets into `pctab`; `nameOff`
into `funcnametab`; `cuOffset` into `cutab`. The runtime reconstructs addresses
with `textAddr(off) = md.text + off` (or via `textsectmap` for multi-section
text). Absolute addresses live only in `moduledata` (Section 1.5) — not in the
tables. History: CL 351463 (Go 1.18) "cmd/link, runtime: use offset for
`_func.entry`" (−10 % relocations on darwin/arm64, −0.64 % size); CL 352191
"remove functab relocations" (−18.3 % relocations, −0.97 % size)
(https://groups.google.com/g/golang-codereviews/c/_VbREqYLSG4,
https://groups.google.com/g/golang-codereviews/c/qglubYm_0gE). The original
motivation was #36313 "runtime: pclntab is too big".

Measured churn for `libcode3` (function grew 7 bytes, nothing moved): `pctab`
1,000,062 differing bytes at same offset but only **581 B** as a zstd patch (pure
shift); `functab+_func` 38,891 differing bytes → 52.9 KB patch (every later
`_func`'s pctab offsets changed by a small constant, spread over 15k structs);
`funcnametab/cutab/filetab/go:func.*/findfunctab` unchanged. For `libcode2`
(+32 shift of half the text) `_func` churn is 67.8 KB and `findfunctab` 1.6 KB.

### 1.5 `runtime.firstmoduledata` — where the absolute addresses are

`ld/symtab.go` writes moduledata with `AddAddr` (absolute) for: `pcHeader`,
`funcnametab`, `cutab`, `filetab`, `pctab`, `pclntable`, `ftab`, `findfunctab`,
`minpc`, `maxpc`, `text`, `etext`, `noptrdata`, `enoptrdata`, `data`, `edata`,
`bss`, `ebss`, `noptrbss`, `enoptrbss`, `covctrs`, `ecovctrs`, `end`, `gcdata`,
`gcbss`, `types`, `etypes`, `rodata`, `gofunc`, `epclntab`, `textsectmap`,
`typelinks`, `itablinks`, `ptab`, `pkghashes`, `inittasks`, `modulehashes`; plus
`AddUint` sizes/offsets for types/itab offsets and lengths. It is 592 bytes; 12
of them changed in `maincode`. In PIE these become `R_X86_64_RELATIVE`
relocations.

Other absolute pointers in read-only data (non-PIE): `abi.Type.Equal` (func
pointer) and `abi.Type.GCData` (`*byte`) in every type descriptor, `abi.ITab`
`{Inter *InterfaceType; Type *Type; Hash; Fun [1]uintptr}`, `.itablink`
entries, function-value symbols (`pkg.F·f`), pointer-containing static
composite literals. Offsets (position-independent): `Type.Str NameOff`,
`Type.PtrToThis TypeOff`, `Method.{Name,Mtyp,Ifn,Tfn}` (`TextOff` = "offset from
the top of a text section"), `.typelink` (`[]int32` from `runtime.types`).
(https://raw.githubusercontent.com/golang/go/master/src/internal/abi/type.go,
`.../internal/abi/iface.go`.)

### 1.6 Other sections

* `.go.buildinfo`: 32-byte header (`"\xff Go buildinf:"`, ptr size, flags), then
  (Go 1.18+) varint-length-prefixed version string and modinfo string inline
  (`src/debug/buildinfo/buildinfo.go`). With `-buildvcs=auto` (default) the
  modinfo includes `vcs.revision`, `vcs.time`, `vcs.modified` — verified in A via
  `go version -m`. Changes per commit, ~100 bytes.
* `.note.go.buildid`: 4-part ID
  `actionID(binary)/actionID(main.a)/contentID(main.a)/contentID(binary)`
  (`cmd/go/internal/work/buildid.go`); rewritten after link by `updateBuildID`.
* DWARF: compressed since Go 1.11 (`.zdebug_*`), gABI `SHF_COMPRESSED` since Go
  1.19 (https://go.dev/doc/go1.19, #50796), DWARF 5 since Go 1.25
  (https://go.dev/doc/go1.25). `-ldflags=-compressdwarf=false` disables
  compression. Every DWARF section carries absolute addresses (`DW_AT_low_pc`,
  line program, `.debug_addr`, `.debug_frame` FDEs, range/loc lists).
* `.symtab`: 24-byte entries with absolute `st_value`; shifts change one entry
  per moved symbol (7 KB zstd patch for 7,281 moved symbols) — cheap, but `-s`
  removes it and `.strtab` anyway.
* PIE / external linking add `.rela` (`.rela.dyn`), `.rela.plt`, `.dynamic`,
  `.dynsym/.dynstr`, `.got(.plt)`, `.data.rel.ro*`, and with an external linker
  the host linker's own layout (`.interp`, `.eh_frame`, …).

## 2. Impact of small changes

### 2.1 Growth and alignment

`assignAddress` aligns every function to 32 bytes on amd64, so a function's
growth is absorbed if `ceil(old/32) == ceil(new/32)`. `formatLogMessage` 773 →
780 B (`libcode3`): absorbed (0 symbols moved, 750 bytes of `.text` differ, 0
bytes of `.rodata`). 773 → 820 B (`libcode2`): 800 → 832 padded, so all 7,281
later functions shifted by exactly +32 (mergeset, storage, promscrape, …, `main`,
FIPS). For a uniformly distributed growth *g* bytes the absorption probability is
roughly `1 − g/32` — an ~8-byte edit is absorbed ~75 % of the time (extrapolation
from the alignment rule, not measured over many edits).

`-ldflags=-funcalign=64` (Go 1.25+) doubles the padding budget: the `libcode2`
growth was absorbed (`.text` differed by 0.78 %, all from `.rodata` reordering),
patch 202 KB vs 332 KB zstd, at +1.4 % file size (+3.2 % `.text`). It is a dice
roll per edit, so it is not a strategy, just a mild bias.

### 2.2 What ripples when N functions shift by +32

Measured on `libcode2` (stripped, so DWARF is out of the picture; per-section
zstd patch):

| section | mechanism | patch |
|---|---|---|
| `.text` | 7,281 moved functions (pure shift, cheap) **plus** 9,410 unmoved functions whose `CALL rel32` / `LEA/MOV rip+disp32` operands cross the boundary: 128,110 changed bytes in 114,519 runs (avg 1.12 B — the low byte of a 32-bit displacement) | 248 KB |
| `.gopclntab` | `functab.entryoff` and `_func.entryOff` for moved functions; `pctab` shift → `_func` pc-offsets; `findfunctab` buckets | 71 KB |
| `.rodata` | new 15-byte string inserted into size-sorted section → 1,421 read-only symbols shift +16/+32 (starting at the 16-byte size class: `cmp..dict.Less[uint64]`, …); function-value symbols and itabs pointing at moved code | 12 KB |
| `.symtab` | `st_value` of moved symbols | 7 KB |
| `.data`, `.noptrdata`, `.itablink`, `.go.module`, `.go.fipsinfo`, `.note.*` | pointers to moved code/data; hashes; build ID | < 4 KB total |

The `.text` cost is not in the moved bytes (delta encoders handle a constant
shift) but in the ~115k one-byte reference edits scattered through unmoved code.
bsdiff, whose "diff" blocks store bytewise differences that compress to near
zero when the difference is a constant, does best here (119 KB vs 332 KB zstd
vs 642 KB xdelta3), which is the same observation Chromium made when building
Courgette/Zucchini
(https://chromium.googlesource.com/chromium/src/+/main/components/zucchini/README.md).

### 2.3 What ripples when a constant changes

Same length (`strsame`, `imm`): nothing moves; 105–106 differing bytes in the
whole 25 MB file (the literal bytes, the build ID, the FIPS hash). Patch ≈ 0.5 KB.

Different length (`strgrow`, +6 bytes): the literal moves within `.rodata`'s
size order; only symbols with sizes between old and new size shift, so 19
functions' displacements changed, 18.7 KB of bytes differ, patch 896 B. File
size unchanged (segment rounding absorbed it).

New literal (`libcode2`, `maincode`): inserts into a size class, pushing every
larger read-only symbol → hundreds of KB of raw byte changes across `.rodata`
and `.text`, ~15 KB after delta encoding.

### 2.4 Inlining fan-out

`libcode` edited `logger.Infof`, a 1-line function the compiler inlines at every
call site: 5,414 functions moved and 5,318 more changed bytes; stripped patch 246
KB zstd — comparable to `libcode2` although the source edit was smaller. PGO
(`-pgo=auto`, `default.pgo`) makes inlining decisions profile-dependent
(https://go.dev/doc/pgo: "the compiler may decide to more aggressively inline
functions which the profile indicates are called frequently"; Go 1.21 added PGO
devirtualization, https://go.dev/doc/go1.21). The PGO design doc proposed
linker-level function reordering
(https://go.googlesource.com/proposal/+/master/design/55022-pgo-implementation.md);
I could not confirm that layout optimizations shipped, but inlining changes alone
make a refreshed profile a whole-binary change. If PGO is used, keep the profile
fixed across the releases you intend to delta.

### 2.5 PIE

With `-buildmode=pie` (internal linking on linux/amd64), `UseRelro()` is true
(`ld/target.go`: PIE/shared/c-archive/c-shared/plugin on ELF), so pointer-bearing
read-only data (types, itabs, funcvals, typelink, itablink) moves to
`.data.rel.ro*` in a `GNU_RELRO` segment; `.rodata` shrinks from 3.50 MB to
1.88 MB. `amd64/asm.go adddynrel` turns every `R_ADDR` in data into an
`R_X86_64_RELATIVE` entry ("r_offset = s + r.Off … r_addend = targ + r.Add") and
"still apply[s] it statically, so in the file content we'll also have the right
offset" — i.e. the address is stored twice (slot + 24-byte rela). A: 66,170
relocations, `.rela` = 1,588,080 bytes.

Delta effect (stripped `libcode2`): `.rela` 33.5 KB + `.data.rel.ro` 27.2 KB of
extra patch, `.text`/`.gopclntab` unchanged, `.rodata` reshuffled but cheap. Total
397 KB vs 332 KB (zstd), 131 KB vs 119 KB (bsdiff). **PIE is mildly worse and
never better** for our purposes; the relocation table does not "absorb"
anything, it just moves the absolute addresses to a different section. Linux is
`exe` by default (`internal/platform.DefaultPIE`; darwin/amd64 became PIE in Go
1.22, windows in 1.15); some distro toolchains patch the default, so pin
`-buildmode=exe` explicitly.

### 2.6 DWARF

Every variant that touched a function changed 95–99 % of every compressed DWARF
section's bytes and produced a ~5 MB patch regardless of the edit. Uncompressed
(`-compressdwarf=false`) the sections are large (17.5 MB) but shift-friendly:
`.debug_info` differed 2.9 %, `.debug_line/loclists/rnglists` 82–88 % at same
offset but the zstd patch fell to 700 KB. If DWARF must ship, ship it
uncompressed or handle it separately (Section 7).

## 3. Build reproducibility

* Go 1.21+: "perfectly reproducible" toolchain builds; the blog enumerates the
  removed inputs (map/goroutine ordering, timestamps, source directory via
  `-trimpath`, host C toolchain via `CGO_ENABLED=0`, dynamic-linker path,
  uid/gid, host OS/arch details) and reports bit-for-bit matches against Ubuntu's
  independently built packages (https://go.dev/blog/rebuild).
* Build ID (`cmd/go/internal/work/buildid.go`): action ID = hash of the action's
  inputs (tool build ID — "for releases we use the release version string
  instead of the compiler binary's content hash" — flags, import paths, source
  hashes, working directory); content ID = hash of the output; the linker is
  run, the output hashed, then the ID is rewritten in place. `-ldflags=-buildid=`
  writes an empty ID. Known non-reproducibility: the action-ID half depends on
  the build directory even with `-trimpath` (#34186, closed as duplicate of
  #33772: "This difference is due to the .note.go.buildid section … It can be
  set to something static e.g. -ldflags=-buildid= (empty string)"; #76743, Go
  1.25.3, same symptom, closed).
* Inputs that must match for bit-identical user binaries: toolchain version,
  `GOOS/GOARCH`, `GOAMD64` (v1 default; recorded in `.go.buildinfo`), `GOFLAGS`,
  `-tags`, `CGO_ENABLED=0`, `-trimpath`, the module graph (`go.sum`), VCS state
  (`-buildvcs`; `vcs.modified=true` on a dirty tree changes `.go.buildinfo`),
  `-X` values, PGO profile, `-race/-msan/-asan`, `-linkmode`. GoReleaser's recipe
  is `CGO_ENABLED=0`, `-trimpath`, `mod_timestamp={{.CommitTimestamp}}`, and no
  time-based `-X` (https://goreleaser.com/blog/reproducible-builds/).
* Observed: two consecutive builds of A were byte-identical, with and without
  `-buildid=`. I did not test a second machine; per the sources above, with the
  same toolchain tarball and the flags listed, the only expected difference is
  the build-ID note if the path differs.

For "verify target integrity against an authoritative hash", this means the
hash of the *release artifact* is authoritative, and a rebuild elsewhere can be
expected to match only if the build-ID trap is neutralized.

## 4. Knobs that affect delta-friendliness

| knob | effect on size (A) | effect on delta | cost / caveat |
|---|---|---|---|
| `-ldflags="-s -w"` | −29.3 % (25.3 → 17.9 MB) | 5.2 MB → 0.12–0.33 MB per one-line change | no DWARF; panics/pprof still work via pclntab. `-s` implies `-w` (Go 1.26 docs) |
| `-ldflags=-w` only | keeps `.symtab/.strtab` (1.6 MB) | symtab churn is ~7 KB | fine if `nm` on the target matters |
| `-ldflags=-compressdwarf=false` | +34 % | DWARF becomes shift-friendly: 700 KB zstd | only if DWARF must be in-file |
| `-trimpath` | ≈0 | needed for reproducibility; removes paths from DWARF/pclntab file table | — |
| `-ldflags=-buildid=` | −36 B | removes a per-build 83-byte change; fixes path-dependent IDs | loses `go tool buildid` |
| `-buildvcs=false` | ≈0 | removes `vcs.*` from `.go.buildinfo` | loses provenance; better to keep it |
| `-buildmode=pie` | +6.3 % | worse (+20 % patch) | avoid unless required |
| `-linkmode=external` | + host-linker sections | unknown; adds a second layout engine | avoid; internal linking is the default for `CGO_ENABLED=0` |
| `GOAMD64=v2..v4` | small | changes codegen once; stable if pinned | pin it |
| `-pgo` / `default.pgo` | small | hostile when the profile changes | freeze the profile across delta'd releases |
| `-ldflags=-randlayout=N` (Go 1.23+) | 0 | randomizes function order — hostile | exists to fuzz layout-dependent bugs (`cmd/link/testdata/script/randlayout_option.txt`); irrelevant to us |
| `-ldflags=-funcalign=64` (Go 1.25+) | +1.4 % | absorbs more growth; probabilistic | small, real, but not a design basis |
| `-ldflags=-R quantum` | rounds segments | not padding of functions | no help |
| `-linkshared` | stdlib in `libstd.so` | app code only in the binary | "experimental", requires `-buildmode=shared` std install; not practical |
| `-a` | none | none | just disables the cache |

There is no linker option to pad individual functions or sections, and no
"stable ordering" option is needed — ordering is already deterministic
(Section 1.2). Anything that reorders (`-randlayout`, PGO profile changes,
adding an import that changes `postorder`) is the enemy.

## 5. Runtime constraints for in-place upgrade (Linux)

Verified with a small Go program (`scratchpad/exp/exec`):

* `cp new prog` onto a running executable: **`cp: cannot create regular file
  'prog': Text file busy`** — `open(2)`: "ETXTBSY: pathname refers to an
  executable image which is currently being executed and write access was
  requested" (https://man7.org/linux/man-pages/man2/open.2.html).
* `mv new prog` (rename over) succeeds; the running process keeps the old
  inode.
* `readlink /proc/PID/exe` → `/…/prog (deleted)`; `proc_pid_exe(5)`: "If the
  pathname has been unlinked, the symbolic link will contain the string
  '(deleted)' appended … attempting to open it will open the executable. You can
  even type /proc/pid/exe to run another copy of the same executable that is
  being run by process pid" (https://man7.org/linux/man-pages/man5/proc_pid_exe.5.html).
  Observed: `/proc/PID/exe id` printed **"I am OLD"**; `./prog id` printed
  **"I am NEW"**. A re-exec that wants the new release must exec the *path*,
  not `/proc/self/exe`.
* `os.Executable()` on Linux is `Readlink("/proc/self/exe")` with
  `stringslite.TrimSuffix(path, " (deleted)")` (`src/os/executable_procfs.go`);
  after the rename it returned the plain path, which now names the new file. The
  doc warns "There is no guarantee that the path is still pointing to the
  correct executable." So `syscall.Exec(os.Executable(), …)` gets the new binary
  — exactly the tableflip/`systemd` style upgrade — and `syscall.Exec("/proc/self/exe", …)`
  gets the old one (useful for a rollback path).
* Executing a verified image from memory: `golang.org/x/sys/unix.MemfdCreate`
  (`MFD_CLOEXEC|MFD_ALLOW_SEALING`, then `F_ADD_SEALS` with
  `F_SEAL_SHRINK|F_SEAL_GROW|F_SEAL_WRITE|F_SEAL_SEAL`), write the bytes, then
  `syscall.Exec("/proc/self/fd/N", argv, env)`. x/sys has no `fexecve`/`execveat`
  wrapper (checked pkg.go.dev); the `/proc/self/fd` path is the standard route.
  After that, `/proc/self/exe` reads `/memfd:name (deleted)` and
  `os.Executable()` returns `/memfd:name`, so anything that re-execs by path must
  be told the real on-disk path explicitly.
* Write-then-exec race (#22315, still open, Backlog): a fd opened for writing in
  one thread can leak into another thread's fork child and make `execve` fail
  with `ETXTBSY` until that child execs. Russ Cox: "This race is going to happen
  every time anyone writes a program that both writes and executes a program."
  Mitigation for binsync: write, `fsync`, **close**, then exec in a context
  without concurrent forks (or retry on `ETXTBSY` with backoff, as `cmd/go` once
  did).
* Signature verification, short notes: `aead.dev/minisign` (pure Go, compatible
  with jedisct1/minisign signatures, Ed25519 + BLAKE2b-512 prehash, v0.3.0);
  `github.com/sigstore/sigstore-go` for cosign/Sigstore bundles (heavier:
  Fulcio/Rekor); `crypto/ed25519` in the stdlib if we control both ends. Verify
  on the assembled image (hash of the full file) before rename.

## 6. Stdlib pieces binsync can rely on

* `debug/elf`: section table, program headers, `SHF_COMPRESSED` decompression
  via `Section.Data()`, symbols, relocations. Pure Go, no cgo.
* `debug/gosym`: `NewLineTable(pclntab, textAddr)` + `NewTable(nil, lt)` gives
  `Table.Funcs` (`Entry`, `End`, `Name`), `PCToLine`, `LineToPC`. Handles
  pclntab versions 1.2, 1.16, 1.18, 1.20 (magic-detected;
  `src/debug/gosym/pclntab.go`); for ≥1.18 entries are `textStart + uint32
  offset`, so `textAddr` must equal `runtime.text` = the `.text` section
  address for internally linked binaries. **Verified on the `-s -w` binary**:
  `.symtab` entries = 0, `gosym funcs = 15,388`, `main.main` resolved to
  `main.go:39`. It does not expose funcdata/pcdata beyond line/SP tables, and
  for pre-1.26 PIE binaries the section is named `.data.rel.ro.gopclntab`.
* `debug/buildinfo`: `ReadFile`/`Read` → `runtime/debug.BuildInfo` (Go version,
  module path/version, deps, build settings incl. `-trimpath`, `-buildmode`,
  `CGO_ENABLED`, `GOAMD64`, `vcs.*`). Cheap way to sanity-check that the
  target's binary was built with the expected flags before diffing.
* `debug/dwarf`: only needed if DWARF is kept.
* `golang.org/x/sys/unix`: `MemfdCreate`, `Fcntl` seals, `Renameat2`,
  `Fsync`. `golang.org/x/arch/x86/x86asm` if we ever do reference-aware
  encoding (not stdlib, but Go-project maintained).
* `go tool nm -n -size`, `go tool objdump`, `readelf`, `bloaty`,
  `github.com/Zxilly/go-size-analyzer` (works on stripped binaries "but may
  lead to inaccurate results"; has a diff mode), `github.com/bradfitz/shotizam`
  for analysis; none are runtime dependencies.

## 7. Implications for binsync

Ranked by expected payoff per unit of effort, with honest uncertainty.

1. **Require `-s -w` (or at least `-w`) in the release build, and refuse/warn
   otherwise.** Payoff: ~20× smaller patches (5.2 MB → 0.12–0.33 MB) and a 29 %
   smaller full transfer. Effort: nil. Certainty: high (all six variants agree).
   `debug/buildinfo` can't tell us whether DWARF was stripped, but
   `debug/elf` can (`.debug_info` present and `SHF_COMPRESSED`).
2. **Choose a shift-tolerant delta encoder.** bsdiff-style (bytewise-difference
   "diff" blocks + bzip2/zstd) beat zstd `--patch-from` by 2.8× and xdelta3 by
   5.4× on the stripped one-line change because the dominant residual is
   "+32 in the low byte of 115k displacements". Payoff: 2–5× on the stripped
   residual. Effort: low (library choice; pure-Go bsdiff ports exist, or an
   rsync-style block scheme with a bsdiff-like inner codec). Certainty: high for
   the ranking; bsdiff's 17 s / ~450 MB RAM on 25 MB inputs is a server-side
   cost only.
3. **Section-aware framing.** Split the ELF by section (or at least: text /
   rodata / pclntab / data / everything-else) and delta each separately with
   per-section settings; treat NOBITS and note sections trivially; never let a
   change in one section desynchronize the encoder's match window in the next.
   Payoff: modest on its own (sum of per-section zstd patches ≈ whole-file
   patch in our data), but it is the scaffolding for items 4–6 and for
   verifying per-section hashes. Effort: low. Certainty: medium — measured
   per-section sums were within ±1 % of whole-file patches, so this is about
   enabling the next steps, not an immediate win.
4. **Reference-aware `.text` encoding (Zucchini/Courgette idea, Go-flavoured).**
   Use `debug/gosym` for function boundaries and an x86-64 decoder (or a
   conservative rel32 scanner: `E8`/`E9`/`0F 8x` + RIP-relative ModRM) to
   convert `rel32`/`disp32` operands to absolute targets, then to *symbolic*
   targets ("function #k + delta", "rodata symbol #j + delta") using both
   binaries' symbol tables from pclntab/`.rodata` layout; diff in that
   representation; re-encode on the target. In our data the information content
   of the `.text` change is ~115k references × ~1 bit ("did it cross the
   boundary") — tens of KB at most vs 119–248 KB today. Payoff: potentially
   5–10× on the post-strip residual (uncertain: rodata references have no
   symbol table without `.symtab`; disassembly must be exact or fall back per
   function; Go's code has no relocations left to guide us). Effort: high.
   Verify by whole-file hash; fall back to raw blocks on any mismatch.
5. **pclntab-aware encoding.** The `_func` array churn (50–70 KB) is
   `uint32` offsets moved by a constant; `functab.entryoff` likewise. A
   transform that decodes `pcHeader`, treats `pctab` as an opaque blob (already
   delta-friendly), and encodes `_func` fields as deltas against the previous
   entry would shrink this to a few KB. Payoff: ~50 KB per change (15 % of the
   stripped residual). Effort: medium (format is stable within a Go minor
   version but has changed in 1.16/1.18/1.20/1.26; gate on magic).
6. **Handle DWARF if it must ship**: either mandate `-compressdwarf=false`
   (patch 700 KB, file +34 %) or decompress `SHF_COMPRESSED` sections before
   diffing and re-compress on the target. Re-compression must be bit-exact to
   preserve the file hash — Go's linker uses `compress/zlib` at default level,
   so a Go-written agent with the same stdlib version can reproduce it, but that
   is version-fragile; fall back to verifying per-section hashes of the
   *decompressed* content plus a final re-hash. Payoff: 7× vs today, only if
   DWARF is required. Effort: medium. Certainty: low on bit-exactness.
7. **Build-side hygiene the release pipeline should enforce** (cheap, but each
   is a policy, not code): `CGO_ENABLED=0`, `-trimpath`, `-buildmode=exe`
   pinned, `GOAMD64` pinned, `-ldflags=-buildid=` (or accept an 83-byte churn),
   PGO profile frozen across releases you intend to delta, no time-based `-X`.
   Reproducibility (Section 3) lets the server derive "expected hash" from the
   artifact rather than from a rebuild.
8. **Do not chase alignment/padding.** `-funcalign=64` gained 40 % on one lucky
   edit for +1.4 % size; the next edit may not be lucky. `-randlayout` is a
   test tool. There is no exposed per-function padding.
9. **Apply side**: assemble to a temp file on the same filesystem, verify hash
   and signature, `fsync`, close, `rename(2)` over the target, then re-exec by
   path (`os.Executable()`), never via `/proc/self/exe`. Keep the old inode
   reachable (`/proc/self/exe` or a hard link) for rollback. Consider
   `memfd_create` + seals + `exec /proc/self/fd/N` when the deployment wants
   "run exactly the bytes we verified" semantics. Mind the ETXTBSY race if the
   agent forks concurrently.

Open questions worth a follow-up experiment: (a) how patch size scales with
binary size and with the number of moved functions (we have one binary and one
shift size); (b) whether an rsync-style rolling-hash chunker with 32-byte-aware
boundaries recovers most of bsdiff's advantage at a fraction of its memory;
(c) Go 1.27's closure-name change ("the compiler chooses the same name for the
function literal regardless of inlining … may also combine multiple instances",
https://go.dev/doc/go1.27) as a source of cross-release layout drift.

## References

Primary sources (Go):
- `cmd/link/internal/ld/data.go` — `textaddress`, `assignAddress`, `dodataSect`, `allocateDataSections`, `address` (local: `/usr/local/go/src`, Go 1.26.4; master: https://raw.githubusercontent.com/golang/go/master/src/cmd/link/internal/ld/data.go)
- `cmd/link/internal/ld/lib.go` — `loadlib`, `postorder`; `cmd/link/internal/loader/loader.go` — `AssignTextSymbolOrder`; `cmd/link/internal/ld/target.go` — `UseRelro`; `cmd/link/internal/ld/symtab.go` — moduledata writer; `cmd/link/internal/ld/pcln.go`; `cmd/link/internal/ld/main.go` — flags (`-randlayout`, `-funcalign`, `-buildid`, `-compressdwarf`); `cmd/link/internal/amd64/{l.go,obj.go,asm.go}`
- `cmd/internal/objabi/pkgspecial.go`; `cmd/internal/obj/plist.go` (`InitTextSym`); `cmd/compile/internal/gc/{main.go,compile.go}`; `cmd/go/internal/work/buildid.go`
- `runtime/symtab.go` (moduledata, pcHeader, `_func`, `textAddr`/`textOff`); `internal/abi/{type.go,iface.go}`; `internal/platform/supported.go` (`DefaultPIE`); `os/executable_procfs.go`
- `debug/gosym/pclntab.go`; `debug/buildinfo/buildinfo.go`
- Linker flag docs: https://pkg.go.dev/cmd/link
- Release notes: https://go.dev/doc/go1.19 (SHF_COMPRESSED), https://go.dev/doc/go1.21 (PGO GA), https://go.dev/doc/go1.22 (darwin/amd64 PIE), https://go.dev/doc/go1.25 (`-funcalign`, DWARF 5), https://go.dev/doc/go1.26 (layout changes), https://go.dev/doc/go1.27
- Reproducible builds: https://go.dev/blog/rebuild; issues https://github.com/golang/go/issues/34186, https://github.com/golang/go/issues/33772, https://github.com/golang/go/issues/76743, https://github.com/golang/go/issues/59525
- pclntab: https://github.com/golang/go/issues/36313; CL 351463 https://groups.google.com/g/golang-codereviews/c/_VbREqYLSG4; CL 352191 https://groups.google.com/g/golang-codereviews/c/qglubYm_0gE; https://github.com/golang/go/issues/76038
- PGO: https://go.dev/doc/pgo; https://go.googlesource.com/proposal/+/master/design/55022-pgo-implementation.md
- `-randlayout` test: https://raw.githubusercontent.com/golang/go/master/src/cmd/link/testdata/script/randlayout_option.txt (flag present from the go1.23 branch)
- ETXTBSY race: https://github.com/golang/go/issues/22315, https://github.com/golang/go/issues/22220

Linux / other:
- https://man7.org/linux/man-pages/man2/open.2.html (ETXTBSY); https://man7.org/linux/man-pages/man5/proc_pid_exe.5.html
- https://pkg.go.dev/golang.org/x/sys/unix#MemfdCreate; https://pkg.go.dev/os#Executable; https://pkg.go.dev/debug/gosym; https://pkg.go.dev/debug/buildinfo
- https://github.com/cloudflare/tableflip; https://pkg.go.dev/aead.dev/minisign; https://github.com/sigstore/sigstore-go
- Zucchini design: https://chromium.googlesource.com/chromium/src/+/main/components/zucchini/README.md
- Size analyses: https://blog.howardjohn.info/posts/go-binary-size/; https://dr-knz.net/go-executable-size-visualization-with-d3-2021.html; https://github.com/Zxilly/go-size-analyzer; https://github.com/bradfitz/shotizam
- GoReleaser reproducible builds: https://goreleaser.com/blog/reproducible-builds/

Experiment artifacts (this session's scratchpad, not committed):
`scratchpad/exp/{driver.sh,driver2.sh,secdiff.py,secpatch.py,pclnparts2.py,ov/*,*.log}`.
