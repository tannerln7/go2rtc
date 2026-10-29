# PLAF203 transport contract

Implemented against `AF203_FW_ota2_e32acf31` (3.1.48), SHA-256
`da353ba37a7d8c1c0790827e5ec267a180519affb5666ae4efd93e5362e00b71`.
Function addresses below refer to that application, not an identically addressed
library. Supporting analysis lives in the private `plaf203-firmware-reconstruction`
repository and the 2026-10-29 camera transport investigations. No vendor source
or private capture is included here. Tests use sanitized native layouts.

## Roles and ownership

`transport.go` owns envelope validation, AV epochs and reliable controls;
`recv.go` owns receive reports; `bootstrap.go` uses those same operations before
runtime goroutines start. During runtime, `innerMu` serializes protocol mutations
and control transmission; ACK snapshots additionally use `ackMu`. The packet
reader rejects unrelated UDP endpoints and the decoder verifies the physical
session nonce, direction, format and lengths. A new `Client` owns a new epoch.

Media assembly (`assembler.go`, `audio.go`) owns codec interpretation, frame
completeness and real-time loss handling. `camera.go` owns SPS readiness, HD
stabilization and clock normalization. Producer/RTSP consumer lifetime does not
own the physical protocol or rewrite ACK state.

## Reset is not quality selection

Reliable family `0x1000`, opcode `0x24` is receive-reset, not SETSTREAMCTRL.
The body is `u32 opcode, u16 request_id, u16 boundary, u32 frame_word`.
Native handler `0x003b4c48` clears outbound AV queues, sets next AV sequence to
`boundary+1`, resets BBR and sends `0x25` with ID/boundary/current frame position.

Bootstrap resets to boundary `0x3fff` for **both** qualities and correlates the
completion's family, body length, request ID and exact boundary before accepting
an AV epoch. The fixed three-second bootstrap deadline fails startup if completion
never arrives; it never guesses an origin. Reliable request retransmissions keep
the original sequence, message group and reset ID. The transport receipt ACK for
a request is not semantic reset completion.

The former `qualityHD`/`qualitySD` captured bodies instead requested reset floors
`0x3fff` / `0x818a`. In particular, `0x818b` was not evidence of a camera-global
SD counter. First-video anchoring heuristics and those templates are removed.

After reset, application queries, data-delay, START and optional AUDIOSTART retain
their observed order. Reset now completes before START. Every media datagram
received during bootstrap passes through the runtime classifier/receipt set;
media reordered ahead of reset completion is held in a bounded queue and checked
against the confirmed sequence/frame floors. Bootstrap never positively ACKs
everything below a collected maximum. Shutdown sends STOP with the next reliable
sequence/message group, not a reused sequence zero.

### What `quality` actually means

The PLAF203 application worker at `0x0002f7b4` implements START/STOP, audio and
playback commands, but **does not implement a per-viewer quality setter** at the
standard `0x0320` or the internal `0x24` ID. The live video pump (`0x0002e948`)
selects pre-encoded 1920x1080 or 640x360 feeds using shared application resolution
state. Application configuration and queue occupancy affect that selector.

`quality=hd` remains a readiness preference: the camera adapter waits for HD SPS
stabilization within its existing bound, then uses the actual available source.
`quality=sd` avoids that HD wait and uses the available source immediately. It
does **not** force 640x360 or send a guessed setter; neither option is a promise
that the OEM's shared selector cannot change. To require SD, configure the feeder's
resolution through its supported application integration, or explicitly transcode
downstream. SPS, not reset bytes or FRAMEINFO online-client count, is authoritative.

## Transport receipt versus media interpretation

Native references: `0x003b5374` dispatch, `0x003b4184` receipt, `0x003be4f8`
checksum and `0x003be8c8` retransmit normalization/FEC staging.

For FEC-wrapped inner type `0x0c`, media starts at offset 16 (normal 36-byte
header) or 24 (timing flag `0x08`, 44-byte header). The 20-byte media header is:

| Relative offset | Field |
| --- | --- |
| 0 / 1 | Channel / final-fragment flag |
| 2 | u16 AV sequence shared by audio and video |
| 4 | **u16** total fragments |
| 6 | u16 fragment index, or metadata length on the final fragment |
| 8 / 10 | u16 payload length / additive checksum |
| 12 / 16 | u32 shared frame group / media counter |

Channels `3/5/7` are audio/key/inter originals; `4/6/8` are retransmissions.
Normalize channel before checksum calculation, as native FEC staging does.
Checksum covers the canonical 20-byte header (checksum bytes zero) and declared
payload, not padding. Reject truncated, inconsistent, oversized or corrupt
envelopes. The application bounds are 1024 bytes per fragment and at most 257
fragments (256 KiB video plus metadata). FEC parity channel `0x0c` has **no AV
position** and is never ACKed as media. This receiver does not reconstruct FEC
parity; missing source data is NACKed normally.

A validated source-data envelope in the current epoch contributes receipt even
if its bytes are not usable H.264/AAC. ADTS sync, recognized metadata, codec
decoding, dropped access units and output backpressure are downstream decisions,
not packet-loss evidence. Retransmissions may close a receipt hole after their
media became too late to use. Duplicate receipt never emits duplicate media.

Frame assembly uses the explicit end flag and metadata length, not a trailer-like
payload suffix to decide whether an arbitrary data fragment ended a frame. Known
video metadata supplies camera timestamps; unknown metadata is conservatively
not emitted as usable H.264. AAC is assembled across fragments and checked as
complete ADTS before publication, including declared lengths and codec parameters.
Incomplete/damaged audio does not fabricate ACK holes or emit partial audio.

## Clear/reset direction and idempotency

Native control implementation: `0x003b4c48`, request helpers `0x003b7964` and
`0x003b758c`.

- Incoming `0x22` means the peer discarded its outbound media through the
  supplied sequence/frame boundary. Purge covered receive/assembly state,
  advance the explicit discard floor, preserve later receipt evidence, require
  a fresh usable GOP and reply `0x23`. A discard is not an assertion of receipt.
- Incoming `0x24` resets the **peer's receive direction**, not ours. This client
  sends no AV/talkback data, so reply `0x25` for the requested outbound boundary
  with empty-sender frame position `0xffffffff`. Do not reset inbound video.
- Incoming `0x25` completes only our matching outstanding receive-reset.
  Stale/duplicate/unrelated completions cannot reset a live receive epoch.
- We never initiate sender-clear (`0x22`): there is no outbound AV queue to
  discard, so unsolicited `0x23` has no outgoing operation to complete.
- Same-ID/same-body peer requests repeat their semantic response without
  repeating state mutation; conflicting or older IDs do not discard newer work.

Reliable fragments are copied into bounded owned storage, reassembled in
sequence, then dispatched. Reliable ACK progress is independent of AV holes.
Unsupported application messages can be transport-delivered without becoming
AV data or causing an unsafe internal operation.

## ACK progression and feedback

Native ACK builder `0x003b3d48` uses highest receipt, remembered gaps, prior report
base and delivery floor `+0x16a0`. The latter is written by:

- Constructor `0x003a609c` (initial `0xffff`).
- `0x003b6ae4` after the expected complete frame group is assembled. In BBR mode
  it does not simply skip to any newer incomplete group.
- Explicit buffer cleanup/reset `0x003b758c` (including local application-requested
  discards), and clear/reset completions in `0x003b4c48`.

There is no native basis for treating our real-time assembler's `forceDrain()` as
proof of transport receipt or as an application-requested SDK buffer reset. This
implementation conservatively advances the report floor through contiguous
validated receipt, or explicit peer-authorized discard. It does not infer a
discard from codec success, elapsed hole age or a later frame.

Reports describe `(base, high]`, with sorted offsets of genuinely missing source
positions. Positive receipt is advertised once before moving the reported base.
Ingress stays within the unambiguous u16 half-space beyond unresolved receipt;
a packet from a later wrap cannot masquerade as recovery of an ancient gap.
If that boundary is reached, real receipt or explicit clear/reset must restore
progress; elapsed time never supplies missing identity.
NACK capacity, range capacity and u16 report span are bounded; high is truncated
before an unrepresentable hole. Failed local ACK writes do not commit progress;
epoch-tagged snapshots cannot commit into a later reset. The native default
100 ms reporting gate replaces the old unconditional 25 ms ticker. Newly
delivered reliable controls/reset completion can force a prompt report; a media
gap alone does not bypass the gate. Timing probes are answered independently.

Native sender `0x003b5374` first purges whole groups below `base & 0xfff0`, then
conditionally scans the interval using its high-node/reference guards. NACKs
move existing in-flight payload to resend only after strict reference/RTT checks.
Timed retry in `0x003b2b68` is independent, with a `max(RTT,RTO)+margin` gate and
front-of-queue dependency. Tests model these guards: an omitted NACK is not
assumed to free a node unconditionally.

BBR feedback `0x003bbec0` adds `NACK_count*1024` when a report accounts new sent
sample bytes. Persistent old NACKs can therefore repeatedly affect rate
calibration; `0x003bb980`, `0x003bb998`, `0x003bbbe8` feed the selected rate into
pacing/window calculations. Correct receipt and paced feedback avoid *fabricated*
loss and unnecessary 40 Hz repeated feedback. Genuine persistent loss can still
produce repeated NACKs; no complete congestion-controller rewrite or promise
of recovery from absent sender payload is implied.

Actual sender queue bytes—not receiver pending count—drive the occupancy metric
`0x003af88c`. Video sender `0x0002e208` can select SD above 60% and HD at/below
0.1%; queue-full rejection can bypass that evaluation. Neither a pinned ACK nor
our synthetic tests prove the contributor's exact queue/rate/stall sequence.

## Validation and limits

The deterministic suite covers envelope acceptance and rejection, native checksum
normalization, fragmented AAC, counts 255/256/257, genuine loss, fifteen received
but codec-invalid positions, out-of-order/late/duplicate receipt, wrap, explicit
discard, reliable reassembly, reset correlation/duplicates, bounded queues,
failed writes and epoch safety. A UDP loopback bootstrap peer drops the first
reset request, verifies reliable retry and sends bootstrap media with a real gap.
Existing camera/GOP/timestamp and selected RTSP/stream regressions remain required.

The exact contributor failure has not been replayed from its missing packets.
These are wire-contract changes and require real-device qualification before
normal release: cold/warm sessions, both readiness preferences, audio, repeated
opens/closes, real loss/reordering and sustained viewing. Test a development build
with the contributor; do not merge the hole-abandonment PR as a shortcut. A
genuinely lost packet absent from the sender cache is a separate unrecoverable
case, not permission to positively ACK it after a timeout.
