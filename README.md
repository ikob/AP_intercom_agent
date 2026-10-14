# Disclaimer

This is an unofficial, experimental project and is not affiliated with or
endorsed by AIPHONE. Use it entirely at your own risk. It can answer or reject
calls, send door-unlock commands, and store camera images. Misconfiguration,
software defects, or device and protocol changes may unlock an unintended
entrance, interrupt intercom service, or expose private images.

Do not rely on this software as a safety or security mechanism. You are solely
responsible for testing it, securing its deployment, complying with applicable
laws and agreements, and accepting all consequences of its use. To the maximum
extent permitted by law, the authors and maintainers provide no warranty and
accept no liability for any loss, damage, security incident, privacy breach, or
other outcome arising from its use or inability to operate.

---

本プロジェクトは非公式かつ実験的なものであり、AIPHONE との関係、承認、
または保証はありません。利用はすべて自己責任です。本ソフトウェアは通話への
応答・拒否、ドアの解錠命令、カメラ画像の保存を行います。設定ミス、ソフトウェアの
不具合、機器やプロトコルの変更により、意図しない入口の解錠、インターホン機能の
停止、またはプライベートな画像の漏えいが発生する可能性があります。

本ソフトウェアを安全設備または防犯設備として利用しないでください。利用者は、
事前の検証、導入環境の保護、適用される法令や契約の遵守、および利用によって生じる
すべての結果について責任を負います。適用法令で認められる最大限の範囲において、
作者およびメンテナーは一切の保証を行わず、本ソフトウェアの利用または利用不能から
生じる損失、損害、セキュリティ事故、プライバシー侵害、その他いかなる結果についても
責任を負いません。

# AIPHONE Intercom Image Agent

This SIP agent saves a timestamped JPEG snapshot from each allowed incoming
intercom ring. Optional automatic unlocking is controlled separately.

## How to use it

Set `AP_INTERCOM_PASSWORD` in the environment, then run the SIP agent as:

```bash
go run . \
  -username cellphone0 \
  -allowed-caller interphone0 \
  -register-uri sip:cellphone0@<IP address>:5060 \
  -message-uri sip:housing@<IP address>:5060 \
  -entrance-uri sip:housing@<IP address>:5060 \
  -incoming-jpeg-dir /tmp/aiphone-rings \
  -answer-calls=false \
  -capture-images=true \
  -send-messages=false
```

For local VS Code debugging, copy `.env.example` to `.env` and set
`AP_INTERCOM_PASSWORD`. The tracked `.vscode/launch.json` loads that file via
`envFile`; `.env` is ignored by Git. The agent also accepts the same environment
variable outside VS Code when `--password` is omitted. An explicit command-line
flag or JSON configuration value takes precedence.

## SIP monitor probe (experimental)

The probe exercises the outgoing SIP setup and teardown used for video
monitoring. It can also poll the IFBOX HTTP endpoint and atomically write the
latest valid JPEG. It does not send an intercom MESSAGE or transmit RTP.

Stop any other agent registered with the same account, start a packet capture,
then run:

```bash
go run . -config /path/to/config.json \
  -monitor-probe \
  -monitor-uri sip:monitor@192.168.100.25 \
  -monitor-hold 5s \
  -monitor-repeat 1 \
  -monitor-jpeg-out /tmp/aiphone-monitor.jpg \
  -monitor-jpeg-interval 500ms \
  -jpeg-query-s cd188d8e518cb12ad7a644f23928f64f
```

Each iteration performs an authenticated monitor INVITE, reliable provisional
response/PRACK, final ACK, optional JPEG polling during the hold, and BYE. The
JPEG URL is built from the answer SDP connection address and `m=video` HTTP
port. Requests use `Host: ifbox` and the IFBOX CGI query parameter `s` observed
in the official application. Configure that value with `jpeg_query_s` or
`-jpeg-query-s`; its default is the value shown above. The output file is
replaced only after a complete JPEG has been validated.

### Finding `jpeg_query_s`

The `s` value is not supplied by SIP or SDP. Capture the official intercom
application's network traffic, find its HTTP request to
`/cgi-bin/image.cgi?s=<VALUE>`, and copy `<VALUE>` into `jpeg_query_s` or
`-jpeg-query-s`.

The first one or two frames can be valid black 640x480 warm-up images. The log
reports each frame's size, dimensions, hash prefix, and whether it differs from
the first frame. Omit `-monitor-jpeg-out` to run the SIP-only probe.

For repeated probes, the next iteration starts only after the preceding BYE
receives `200 OK`; the same output path is updated with the latest frame. A
successful run ends with `Monitor probe completed ... result=pass` and exits.

## JPEG capture for incoming rings

The registered agent can save an entrance image for every allowed incoming
ring while `capture_images` is enabled. Image capture, automatic unlock, and
whether the call is answered afterward are controlled independently:

```bash
go run . -config /path/to/config.json \
  -answer-calls=false \
  -capture-images=true \
  -send-messages=false \
  -incoming-jpeg-dir /tmp/aiphone-rings \
  -incoming-jpeg-hold 5s \
  -incoming-jpeg-interval 1s \
  -incoming-jpeg-max-files 100 \
  -reject-code 486 \
  -reject-reason "Busy Here"
```

The runtime `capture_images` state controls JPEG capture and requires a
configured `incoming_jpeg_dir`. The runtime `send_messages` state controls
automatic unlock, and its value at startup also gates the initial generic
MESSAGE. `unlock_callers` is a second, caller-specific safety allow-list for the
unlock MESSAGE. The agent snapshots the capture and automatic-unlock states
independently when each allowed ring arrives, so a state change during a call
affects only later calls.

New flag-based configurations default to image capture ON and automatic unlock
OFF. For compatibility, a legacy JSON configuration that explicitly contains
`send_messages` but omits `capture_images` inherits the `send_messages` value
for image capture at startup. Once explicitly configured, `capture_images` and
`send_messages` are independent. HTTP `POST /v1/state` is a partial update:
omitted fields retain their current values.

`answer_calls` remains independent. When it is true, the agent answers after
JPEG capture, waits approximately one second, and sends BYE. When it is false,
the agent sends the configured final rejection instead, and the caller may
display Busy or congestion.

For each qualifying call, the agent sends a reliable `183 Session Progress`
with the audio and HTTP/JPEG SDP. If automatic unlock is independently enabled,
it immediately sends the unlock MESSAGE only when the caller is in
`unlock_callers`. It accepts the IFBOX-specific PRACK and
begins HTTP polling after replying `200 OK` to PRACK. Polling stops before the
final response. It then either answers and sends BYE (`answer_calls=true`) or
sends the configured rejection (`answer_calls=false`). If capture setup fails
before `183` while the application and dialog remain active, an eligible caller
is still unlocked.

One timestamped file is retained per ring, for example
`20261004T214123.123456789+0900_interphone0.jpg`. Frames received during that
ring atomically update only that file, leaving earlier rings intact. This is
important because the external and internal entrances both identify as
`interphone0`; the apartment door identifies as `interphone1` in the tested
installation. Every expected caller must be included with `-allowed-caller`,
unless the allow-list is empty. By default only `interphone0` is in
`unlock_callers`; `interphone1` can be photographed but is never unlocked.

The agent keeps the newest 100 timestamped images by default. Configure the
limit with `incoming_jpeg_max_files` or `-incoming-jpeg-max-files`. Retention
runs at startup and after each successful ring capture; unrelated files,
directories, symbolic links, and images still being written are never removed.
Use a directory dedicated to one running agent; active-file protection and
orphan cleanup are process-local. Cleanup failures are logged but do not
prevent automatic unlock or SIP completion.

For the Home Assistant add-on, use the persistent `/media/ap_intercom_agent`
directory. The add-on maps `/media` read-write, and Home Assistant can browse
the retained images under Media > My media. Consider excluding Media from
automatic backups if these transient entrance images should not enlarge backup
archives.

## Incoming ring webhook

Set `AP_INTERCOM_RING_WEBHOOK_URL`, `ring_webhook_url` in JSON, or
`--ring-webhook-url` to a Home Assistant webhook URL. The environment variable
avoids exposing the secret URL in a process list. The agent sends one JSON
`POST` when it accepts a new SIP `INVITE` from an allowed caller. This
notification is independent of `answer_calls`, `capture_images`, and
`send_messages`. SIP retransmissions with the same Call-ID do not send
duplicate notifications.

```json
{
  "type": "ring",
  "at": "2026-10-15T12:34:56+09:00",
  "caller": "interphone1",
  "call_id": "...",
  "dialog_id": "..."
}
```

The webhook is optional. Delivery runs asynchronously so an unavailable Home
Assistant instance cannot delay SIP call handling. The agent does not retry a
failed request because Home Assistant may already have accepted it; automatic
retries could produce duplicate doorbell events.

Treat the complete webhook URL as a secret. It is redacted from `/v1/config`,
not written to logs, and redirects are not followed. Use an unguessable webhook
ID and keep Home Assistant's webhook trigger local-only.

Home Assistant can turn the webhook directly into a standard doorbell event
entity without a custom integration. Store the same secret ID used in the URL
in `secrets.yaml`:

```yaml
aiphone_intercom_webhook_id: "REPLACE_WITH_A_LONG_RANDOM_VALUE"
```

Then add this trigger-based Template Event to `configuration.yaml`:

```yaml
template:
  - triggers:
      - trigger: webhook
        webhook_id: !secret aiphone_intercom_webhook_id
        allowed_methods:
          - POST
        local_only: true
    event:
      - name: "AIPHONE Intercom Doorbell"
        unique_id: aiphone_intercom_doorbell
        device_class: doorbell
        event_type: ring
        event_types: "{{ ['ring'] }}"
```

For example, if Home Assistant is reachable locally at port 8123, configure
the agent with
`http://127.0.0.1:8123/api/webhook/REPLACE_WITH_A_LONG_RANDOM_VALUE`.
The resulting `event.aiphone_intercom_doorbell` entity reports the standard
`ring` event type.

## On-demand live camera HTTP

The camera endpoints share one in-memory frame source and one outgoing SIP
monitor dialog. The first HTTP camera request starts the monitor dialog and
HTTP/JPEG polling; concurrent viewers reuse it:

- `GET /v1/camera.jpg` returns the latest JPEG and extends the monitor's idle
  lease. A valid black 640x480 placeholder is available during SIP setup.
- `GET /v1/camera.mjpeg` returns a `multipart/x-mixed-replace` MJPEG stream.
  The monitor remains active while at least one stream is connected.

When the last stream closes and no snapshot has been requested for 15 seconds,
the agent sends BYE. A later request creates a new dialog. `HEAD` requests do
not start or extend a monitor session. The monitor target defaults to
`sip:monitor@<proxy host>`; `monitor_uri` or `-monitor-uri` can override it.
Validated incoming-ring frames are published to the same source.

For example:

```bash
curl --fail http://127.0.0.1:18080/v1/camera.jpg --output /tmp/aiphone.jpg
ffplay http://127.0.0.1:18080/v1/camera.mjpeg
```

### Home Assistant camera

Use **Generic Camera** with the optional go2rtc sidecar. The pipeline is
JPEG → HTTP MJPEG → FFmpeg H.264 → go2rtc RTSP. Live monitoring defaults to
two JPEG frames per second; the MJPEG transport does not recompress them.

Install [go2rtc](https://github.com/AlexxIT/go2rtc/releases) and FFmpeg with
`libx264` on the same host as the agent, then run:

```bash
go2rtc -config go2rtc.yaml
```

The checked-in configuration uses loopback and is intended for processes sharing
one host network. It exposes `rtsp://127.0.0.1:8554/intercom`; the Go agent
continues to serve HTTP on port 18080. The sidecar is a separate process and
must be started/supervised separately. No change to the SIP agent flags is needed.

In Home Assistant's **Generic Camera** integration, configure:

- Still Image URL: `http://127.0.0.1:18080/v1/camera.jpg`
- Stream Source URL: `rtsp://127.0.0.1:8554/intercom`
- RTSP transport: TCP
- Username and password: empty for this loopback configuration
- Verify SSL: disabled for the HTTP snapshot URL

If HA, go2rtc, or the agent are in separate containers/hosts, loopback refers to
that container/host. Adjust the source address, RTSP listener, and HA URLs to
reachable trusted addresses. An existing go2rtc add-on can use the `streams` and
`ffmpeg` sections from `go2rtc.yaml`; merge these with its existing configuration
rather than replacing its listeners or other cameras. Check for port conflicts
with an already running go2rtc instance.

The first viewer starts one shared FFmpeg process, which opens the MJPEG URL
and acquires the existing SIP monitor lease. Concurrent viewers share it. After
the last viewer disconnects, go2rtc closes FFmpeg and its HTTP connection; the
agent stops the monitor after the existing 15-second idle period. Keep go2rtc
preloading and HA's **Preload stream** option disabled. Snapshot polling also
extends the monitor lease, so a dashboard repeatedly requesting still images
can keep the monitor active even without an RTSP viewer.

The FFmpeg input uses short probing and wallclock timestamps for sparse JPEGs.
The output uses H.264 baseline/yuv420p at a constant 2 fps with one keyframe per
second, allowing viewers to join promptly. Explicit output timing ensures HLS
players receive correct half-second frame durations; the MJPEG demuxer's
default 25 fps timing can leave Safari showing a blank player. This does not
interpolate motion.
FFmpeg must support `-fps_mode` (FFmpeg 5.1 or newer). This configuration targets
go2rtc 1.9.14. To inspect playback:

```bash
ffplay -rtsp_transport tcp rtsp://127.0.0.1:8554/intercom
```

An opt-in integration test exercises the actual MJPEG handler with synthetic
640x480 JPEGs, two-frame-per-second packet timestamps and durations, concurrent RTSP
viewers, disconnect cleanup, and restart. It does
not connect to the intercom:

```bash
GO2RTC_BIN=/path/to/go2rtc go test -run TestCameraRTSP -v .
```

The existing **MJPEG IP Camera** integration remains usable with
`http://127.0.0.1:18080/v1/camera.mjpeg` and the same snapshot URL. Generic Camera
also accepts the snapshot URL alone, but the raw MJPEG URL is not the H.264
stream source supplied by this setup.

RTSP output does not implement a Matter accessory. Future Matter camera support
requires a Matter implementation and WebRTC session handling; this H.264 stage
provides a reusable video source. WebRTC listening is disabled in the standalone
configuration until that separate integration is configured.

The HTTP camera is video-only. The SIP offer includes the IFBOX-compatible
audio description, but the agent does not transmit RTP or expose audio through
these endpoints. Together with the optional webhook it provides a camera and
standard doorbell event entity, but it is not yet a complete Home Assistant
intercom: it does not provide audio, two-way talk, or native WebRTC. On-demand
monitoring has been verified with real hardware;
publishing incoming-ring frames to the live MJPEG source is implemented and
covered by protocol tests, but still requires end-to-end hardware validation.
The latest real frame remains available between monitor sessions and is
refreshed after the next camera request.

The HTTP control plane and camera endpoints currently have no HTTP
authentication. Although `/v1/config` redacts the SIP password, `/v1/state`
can change capture and automatic-unlock behavior. Keep `http_listen` on
loopback or another trusted interface; do not expose it outside the Home
Assistant host.

## License

The current source is licensed under the
[Apache License 2.0](./LICENSE). Earlier published revisions remain available
under the MIT terms that accompanied them. Third-party dependencies retain
their respective licenses.
