# Cross-architecture audio compatibility verification

The production `render-mixdown` workflow check only proves the service works on
the machine that runs it. The renderer's signal path is floating-point
(`float32`/`float64` in resampling, equal-power crossfade, gain, and peak
normalization) feeding an integer PCM16 encoder, so the same inputs can produce
slightly different output bytes on `amd64` and `arm64` without anything being
wrong — and a genuinely broken build can hide inside a loose tolerance.

This directory adds a repeatable verification that renders **the same
digest-pinned inputs and timelines** through two independently compiled service
binaries, then judges the downloaded results with code that is independent of
the production audio packages.

## What runs

```
scripts/xarch-verify.sh
```

The driver:

1. Cross-compiles `voiceservice` for `linux/arm64` and `linux/amd64` and builds
   the `xarchcheck` tool.
2. Materializes a deterministic input suite (4 WAV sources, 3 render scenarios)
   under `.xarch-results/fixtures/` and pins every file with a SHA-256 digest.
3. Runs each architecture's real service binary against the **same** fixture
   directory over the real HTTP API. A foreign binary runs under a qemu user
   emulator when one is available.
4. Judges the two output bundles and writes `.xarch-results/judge-report.json`.
5. On any failure, writes a self-contained `.xarch-results/failure-case/`.

Exit codes: `0` compatible, `1` meaning-changing difference found, `3` only one
architecture could execute (the other is explicitly left **unverified**).

## Why the inputs are trustworthy on both sides

The fixtures in `internal/xarch/fixtures.go` are synthesized with **integer /
fixed-point arithmetic only** (Q32 phase accumulator, Q15 sine, integer PCM16
codes). There is no floating-point on the fixture signal path, so regenerating
them on any architecture yields byte-identical files. Each materialized WAV is
pinned by digest in `suite.json`; a runner refuses to start if an on-disk
fixture digest does not match the pin, so the two architectures can never be
compared against different input facts.

The scenarios deliberately cover every architecture-sensitive operation:

| Scenario | Coverage |
|---|---|
| `case_resample_stereo` | 44.1k→48k upsampling, mono→stereo, crossfade, normalization |
| `case_crossfade_gain` | 44.1k stereo→48k, positive/negative gain, long equal-power fade |
| `case_stereo_to_mono` | stereo→mono downmix, interior source interval, downsampling |

## How the judge distinguishes acceptable drift from changed meaning

The judge (`internal/xarch/judge.go`) never imports `audiocodec`, `render`, or
any other production package. It re-parses downloaded WAV bytes with its own
integer PCM16 codec and recomputes peak/RMS itself. It uses **tiered** rules,
not one blanket tolerance:

1. **Structural / editing meaning — exact match.** Container shape, sample rate,
   channel count, frame count, byte length, receipt span placement, gains,
   crossfade frames, channel actions, and digest self-consistency must match
   exactly. A shifted or re-edited timeline fails even if samples look close.
2. **Signal level — bounded one-LSB numeric rule.** A per-sample difference is
   accepted only when it is exactly one PCM16 quantization step AND neither side
   is digital silence. This is the smallest difference float32 evaluation-order
   changes can produce at the rounding boundary; the real arm64/amd64 run shows
   exactly this (a handful of 1-LSB samples among tens of thousands).
3. **Meaning-changing signal differences — hard fail.**
   - any delta larger than one LSB (numeric drift beyond rounding),
   - a zero code becoming non-zero or vice versa (silence must stay exact),
   - a polarity flip at non-trivial amplitude (≥ ~ -42 dBFS).
4. **Receipt measurements — independently recomputed, tight epsilon.** Each
   side's receipt digest/frame/peak/RMS is checked against the judge's own
   measurements, and the two receipts must agree within `1e-3` dB (derived from
   LSB-scale movement, not chosen to make the test pass).

A bit-for-bit byte digest comparison is deliberately *not* the pass criterion:
it would reject the legitimate 1-LSB float differences observed between real
architectures. Conversely, a single wide tolerance is never used, so a real
regression cannot be averaged away.

## Replayable minimal failure engineering case

When the judge fails, `failure-case/` contains everything needed to reproduce
it with no other state:

```
failure-case/
  inputs/            # suite.json + every pinned fixture (the shared facts)
  left-bundle-*/     # that architecture's WAV outputs and receipts
  right-bundle-*/    # the other architecture's WAV outputs and receipts
  judge-report.json  # full per-scenario verdict with offending samples
  failure-case.json  # failed cases and the exact replay commands
```

Re-judging is offline and deterministic:

```
xarchcheck judge -fixture-dir failure-case/inputs \
  -left-arch arm64 -left-bundle failure-case/left-bundle-arm64 \
  -right-arch amd64 -right-bundle failure-case/right-bundle-amd64 \
  -report replay-report.json
```

## Running on a host that can only execute one architecture

If neither native execution nor a qemu user emulator exists for one
architecture, the driver does not fabricate a passing result. It records the
runs it could do, writes `conclusion.json` naming the `unverified_arch`, and
exits `3`. The materialized fixtures and the other side's bundle are retained so
the missing side can be rendered later on suitable hardware and judged against
the identical input facts.

## Manual subcommands

```
# Materialize inputs only
xarchcheck fixtures -fixture-dir <dir>

# Render one architecture (add -qemu for an emulated foreign binary)
xarchcheck run -arch amd64 -binary ./voiceservice-amd64 \
  -fixture-dir <dir> -work-dir <work> -bundle-dir <bundle>

# Compare two already-rendered bundles
xarchcheck judge -fixture-dir <dir> \
  -left-arch arm64 -left-bundle <b1> \
  -right-arch amd64 -right-bundle <b2> \
  -report report.json -failure-case-dir <case-dir>
```

## Scope boundary

This verification adds no production code path and changes no production audio
behavior: the original `timelinecheck` workflows continue to pass, and the
renderer source under `internal/audiocodec` and `internal/render` is unmodified.
The fixture, codec, metric, and judge implementations live entirely under
`internal/xarch` and `cmd/xarchcheck`.
