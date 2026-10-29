# Petlibro go2rtc

This repository is a fork of [go2rtc](https://github.com/AlexxIT/go2rtc) with a
clean-room, LAN-only input for Petlibro camera feeders. It connects directly to
the camera over its Kalay/TUTK-family UDP protocol and exposes the resulting
H.264/AAC stream through go2rtc's RTSP, WebRTC, and HTTP outputs.

The Petlibro implementation has been tested with the PLAF203. PLAF103 protocol
captures informed the implementation, but that model has not been verified
end-to-end. This project is not affiliated with Petlibro or TUTK.

## Requirements

- A Petlibro camera that has completed initial provisioning in the Petlibro app
- The camera's 20-character UID
- Network access from the go2rtc host to the camera on UDP port `32761`
- [Go 1.24 or newer](https://go.dev/doc/install) for a source build
- FFmpeg or ffprobe only if you want command-line stream verification

## Install from source

Clone this repository and check out a released `v*-petlibro.*` tag or another
deliberately selected revision. From the repository root:

```bash
go build -o go2rtc .
go build -o petlibro-resolve ./cmd/petlibro-resolve
```

Confirm the binary starts:

```bash
./go2rtc -version
```

## Configure a camera

Copy the tracked example, then edit the copy with your camera's LAN address and
UID:

```bash
cp go2rtc.example.yaml go2rtc.yaml
```

`go2rtc.yaml` is intentionally ignored by Git so device identifiers and local
addresses do not get committed. A minimal fixed-address configuration is:

```yaml
streams:
  petlibro_feeder: petlibro://192.168.1.42?uid=PLAF20300000000ABCD0&quality=hd
```

Replace both placeholder values. If the camera and go2rtc are on the same
broadcast domain, the camera can instead be discovered by UID:

```yaml
streams:
  petlibro_feeder: petlibro://?uid=PLAF20300000000ABCD0&quality=hd
```

For a routed camera network, add one or more `subnet=` query parameters, such as
`subnet=192.168.1.0/24`. Container deployments normally need host networking for
UID discovery; a fixed camera IP avoids broadcast discovery.

See [go2rtc.example.yaml](go2rtc.example.yaml) for documented user options and
[the Petlibro module reference](internal/petlibro/README.md) for supported
behavior and limitations.

## Run

```bash
./go2rtc -config ./go2rtc.yaml
```

The default endpoints are:

| Service | Address |
| --- | --- |
| Web interface | <http://localhost:1984/> |
| RTSP stream | `rtsp://localhost:8554/petlibro_feeder` |
| WebRTC signaling/media | TCP and UDP port `8555` |

The stream name in the RTSP URL must match the key under `streams:`.

To verify the negotiated video codec and resolution:

```bash
ffprobe -v error -rtsp_transport tcp \
  -show_entries stream=codec_name,width,height \
  -of default=noprint_wrappers=1 \
  rtsp://127.0.0.1:8554/petlibro_feeder
```

`quality=hd` is an HD-readiness preference; `quality=sd` skips that HD wait.
Neither sends an OEM resolution setter. The feeder's application configuration
and congestion state select the source; see the [native transport contract](docs/petlibro-transport.md).
An HD camera may initially send a 640x360 SPS before switching to 1920x1080.
The PLAF203 adapter automatically gives this transition a bounded 15-second
preference window before advertising the initial track. This limits startup
latency; it does not guarantee HD, and later SPS changes are forwarded in-band
without changing the initial codec/SDP description. If stabilization consumed
frames after its last candidate IDR, the adapter uses a separately bounded
five-second resynchronization phase to find a new SPS-bearing IDR so delivery
begins at a continuous live GOP rather than replaying an incomplete history.
Requested audio discovery may use a separate readiness interval.

## Build

Build and test the fork directly from the repository root:

```bash
go test ./pkg/petlibro ./cmd/petlibro-resolve
go test ./internal/streams -run '^Test(TimestampContinuity|StreamCorrelationIDs)'
go test ./pkg/rtsp -run '^Test(PacketWriter|PacketWriters|WriteInterleavedData)'
go build -o /tmp/petlibro-go2rtc .
go build -o /tmp/petlibro-resolve ./cmd/petlibro-resolve
```

For the supported container containing both the camera service and PLAF203
controller, use the
[Petlibro Local Home Assistant add-on](https://github.com/tannerln7/Petlibro-Home-Assistant).

## Upstream and releases

The maintained `petlibro` branch preserves the upstream go2rtc history and the
Petlibro camera development history. The original integration was based on
upstream commit `dc1685e9cf7a8c349181f20a1b4a44825ed394c5`; subsequent upstream
synchronizations are merged deliberately and documented in their pull
requests. The Go module path remains `github.com/AlexxIT/go2rtc` to avoid a
high-risk rewrite of go2rtc's internal imports.

Fork releases use tags such as `v1.9.14-petlibro.1`. They are independent of
Petlibro Home Assistant add-on releases. Consumers should pin a full commit or
immutable tag and verify the published checksums rather than tracking the
`petlibro` branch.

## Support, Issues & Troubleshooting

- [Report a reproducible bug](https://github.com/tannerln7/go2rtc/issues/new?template=bug_report.yml)
  with the structured issue form
- [Debugging and troubleshooting discussion](https://github.com/tannerln7/go2rtc/discussions/2)
  for questions, intermittent problems, environment comparisons, and test results
- [Petlibro debugging guide](docs/debugging.md) for health counters, trace
  controls, bounded captures, replay, and common failure modes

Use Discussions while the root cause is uncertain. Open an Issue when a defect
is reproducible or an improvement is clearly scoped, and link any supporting
discussion. Sanitize logs and configuration before posting.

## Development

- [Development guide](docs/development.md) — package architecture, protocol
  invariants, tests, build checks, and live-test workflow
- [Contributing](CONTRIBUTING.md) — useful issue evidence, privacy rules, and
  change-submission checklist
- [Upstream go2rtc documentation](https://github.com/AlexxIT/go2rtc#readme) —
  general output protocols, APIs, transcoding, and integrations

## License

This project retains go2rtc's [MIT license](LICENSE). See the upstream project
for the original implementation and broader go2rtc documentation.
