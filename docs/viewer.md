# Viewer and recorded vehicle telemetry

## Controls

Opening an unencrypted event or drive enters the cinema viewer. Use the thumbnail rail to select Front, Rear, Left/Right repeaters, Left/Right pillars or Grid. Thumbnails reuse the existing decoded video frames; they do not open duplicate video streams. Camera playback and minute selection continue to use the existing synchronized player.

The top controls open clip details, enter/exit fullscreen where supported, download the selected camera's current minute, and close the viewer. Close restores the normal desktop/sidebar or mobile bottom navigation. The details panel contains event information, minute selection and all downloads. Space plays/pauses, left/right arrows seek five seconds, and Escape closes details or the viewer (the browser handles Escape from native fullscreen).

## Low data

The Pi's Wi-Fi carries about 25 Mbit/s, and a minute of HW4 footage needs about 43 for all six cameras (the front alone about 10). So in Full quality only the camera on screen streams; the others keep the frame they loaded, and the grid streams all six and can stall.

**Low data** (next to the speed menu; the browser remembers it) plays every camera's keyframes only. Tesla's cameras write one every half second, so that's 2 frames a second at full resolution, about a fifth of the bytes: roughly 2 Mbit/s for the front camera and 1.1 for each of the others, 7.5 for all six. Every camera plays, in the grid and in the thumbnails, and the next minute buffers for all of them.

The server builds these copies as they're read (`/media/...?keyframes=1`): a new index listing only the keyframes, each lasting until the next, followed by the keyframes' bytes read straight from the clip. Nothing is decoded or re-encoded and nothing is stored, so it starts at once and costs the Pi almost nothing. Each keyframe keeps its decode time, so Low data stays in step with the telemetry and with Full quality, and switching keeps the position. A clip the car was still writing when it was snapshotted has no index yet and can't be shown this way.

## Recorded overlay

The HUD displays gear, speed, steering-wheel angle, accelerator pedal percentage, brake state, left/right turn signals and the recorded assistance mode (Manual, FSD, Autosteer or TACC). Click the speed unit to switch MPH / km/h; the browser remembers the choice when storage is available. The accelerator is the recorded pedal position, not motor power or the vehicle controller's torque request. In an FSD recording the physical accelerator can remain at zero while the car moves.

The details panel adds GPS latitude/longitude, heading, three acceleration axes, frame sequence and schema version. No external map or telemetry service is loaded. Values are local to the recording; they are not live vehicle data. Missing metadata or a gap longer than one second is shown as unavailable, never fabricated as P, zero speed, or a released brake.

## Source and timing

Tesla publishes the [SEI metadata schema](https://github.com/teslamotors/dashcam/blob/master/dashcam.proto) and [availability notes](https://github.com/teslamotors/dashcam). Its documentation identifies firmware 2025.44.25+ and HW3+ as requirements, and says parked clips may omit telemetry. Encrypted car recordings continue to use the existing external-viewer/download path; Glovebox does not bypass encryption.

The Go reader independently implements that published wire schema. It reads H.264 SEI user data and MP4 sample timing / chunk tables, supports 32/64-bit chunk offsets, variable frame durations, zero-duration samples and ordinary trim/initial-empty edit lists. Decode timestamps match the existing Glovebox media route's composition-offset patch. Nonlinear edit lists, fragmented MP4, unsupported codecs and malformed tables are not guessed at. No third-party parser code or new runtime dependency is bundled.

The browser selects the latest recorded sample at or before displayed media time. Video-frame callbacks drive the HUD when supported; timeupdate is the fallback. Seeking backwards and changing minutes select new data, and abort obsolete requests. The browser caches at most six per-file telemetry responses. Thumbnails draw at no more than four refreshes/second; video streams remain the original synchronized set.

## Endpoint and privacy

GET /api/telemetry/{snapshot}/{path...} opens only files already permitted by the clip library and snapshot index. It shares the existing local-network access model of recordings, including sensitive location data. Do not expose the device interface to the public internet. There is no upload to Tesla or an outside telemetry service. Original files are not changed.

Parsing is bounded: MP4 metadata size, table/sample counts, per-frame NAL count and SEI size have limits; concurrent endpoint scans are limited to two and cancellation is checked during extraction. Video pixel data is skipped, not decoded by the server. On-device timing/performance still needs validation on the target board.

## Validation and local previews

- go test ./... runs synthetic parser / timing / path access tests, including malformed input and cancellation.
- node --test scripts/player.test.mjs scripts/web-ui.test.mjs checks timestamp selection, seeking, gaps, units and update notices.
- go test ./internal/telemetry -fuzz FuzzExtract -fuzztime 5s -parallel 2 exercises the binary parser.
- Optionally set GLOVEBOX_TELEMETRY_FIXTURE to a local recording before running go test -v ./internal/telemetry. The private video is not checked into Git.
- go run ./tools/telemetryinspect /path/to/timestamp-front.mp4 prints sample count, time span, speed range and gear counts, without GPS coordinates.
- go run ./tools/uidemo -addr 127.0.0.1:8103 -footage /path/to/isolated-demo -real-clip /path/to/timestamp-front.mp4 -setupdone copies one supplied front recording into an isolated local demo snapshot. Its original timestamp is preserved. Other status in this development server is demo data. The regular multi-camera generated demo remains available without -real-clip.

A locally supplied 60.0827-second, 2,164-frame H.264 recording was used to validate extraction and the real FSD/speed/steering overlay. No private video or telemetry fixture is part of the source changes.
