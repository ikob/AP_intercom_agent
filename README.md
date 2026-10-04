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

# How to use it.

Set `AP_INTERCOM_PASSWORD` in the environment, then run the SIP agent as:

```bash
go run . --username cellphone0 -allowed-caller interphone0 -register-uri sip:cellphone0@<IP address>:5060 -message-uri sip:housing@<IP address>:5060 -entrance-uri sip:housing@<IP address>:5060 -answer-calls=true -send-messages=true
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
  -monitor-jpeg-interval 1s \
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
ring while `send_messages` is enabled. Whether the call is answered afterward
is controlled independently:

```bash
go run . -config /path/to/config.json \
  -answer-calls=true \
  -send-messages=true \
  -incoming-jpeg-dir /tmp/aiphone-rings \
  -incoming-jpeg-hold 5s \
  -incoming-jpeg-interval 1s \
  -incoming-jpeg-max-files 100 \
  -reject-code 486 \
  -reject-reason "Busy Here"
```

The runtime `send_messages` state is the automatic-unlock mode switch. The
agent captures an image only when that state is ON at the instant the ring
arrives and `incoming_jpeg_dir` is configured. The same state snapshot gates
the unlock MESSAGE, while `unlock_callers` is a second, caller-specific safety
allow-list. A Home Assistant state change during the call therefore cannot
split the decisions. `answer_calls` remains independent and should normally
stay true in the tested installation to avoid a Busy/congestion indication.
After JPEG capture, the agent answers, waits approximately one second, and
sends BYE. If `answer_calls` is false, it sends the configured final rejection
instead, and the caller may display Busy or congestion.

For each qualifying call, the agent sends a reliable `183 Session Progress`
with the audio and HTTP/JPEG SDP, then immediately sends the unlock MESSAGE only
if the caller is in `unlock_callers`. It accepts the IFBOX-specific PRACK and
begins HTTP polling after replying `200 OK` to PRACK. Polling stops before the
final response. It then either answers and sends BYE (`answer_calls=true`) or
sends the configured rejection (`answer_calls=false`). If capture setup fails
before `183`, an eligible caller is still unlocked.

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

## License

The current source is licensed under the
[Apache License 2.0](./LICENSE). Earlier published revisions remain available
under the MIT terms that accompanied them. Third-party dependencies retain
their respective licenses.
