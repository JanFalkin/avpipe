# stitch

`stitch` can run in three output modes:

1. **MP4 mux** (default): produces a single progressive MP4.
2. **DASH playlist** (`-dash-dir`): stitches to an intermediate MP4 timeline, then carves that into DASH (`dash.mpd` + segments).
3. **DASH + HTTP server** (`-serve`): same as above, then starts an HTTP file server with CORS headers so a browser-based player can fetch segments directly.

Input modes:

- Legacy flags (`-prog-dir`, `-ad-dir`, `-out`)
- JSON spec (`-spec`) for timeline insertion at arbitrary positions

## Build

```sh
make build-override PKG_CONFIG_PATH=$HOME/.local/lib/pkgconfig
```

From repository root:

```sh
make -C ads/cmd/stitch build-override PKG_CONFIG_PATH=$HOME/.local/lib/pkgconfig
```

## DASH playlist mode

Generate a self-contained DASH directory:

```sh
ads/cmd/stitch/bin/stitch \
  -prog-dir out/prog \
  -ad-dir out/ad_fake \
  -dash-dir out/dash
```

This creates:
- `out/dash/vinit-stream0.m4s`, `out/dash/vchunk-stream0-NNNNN.m4s` — video segments from carving the stitched MP4 timeline.
- `out/dash/ainit-stream0.m4s`, `out/dash/achunk-stream0-NNNNN.m4s` — audio segments from carving the stitched MP4 timeline.
- `out/dash/dash.mpd` — MPEG-DASH manifest.

Works with the `-spec` flag too:

```sh
ads/cmd/stitch/bin/stitch -spec sample_spec.json -dash-dir out/dash
```

## HTTP server mode

```sh
ads/cmd/stitch/bin/stitch \
  -prog-dir out/prog \
  -ad-dir out/ad_fake \
  -serve :8080
```

- `-serve` implies `-dash-dir out/dash` when `-dash-dir` is not set.
- All responses include `Access-Control-Allow-Origin: *` so browser DASH players (e.g. dash.js) work cross-origin.
- The server runs until killed with Ctrl+C.

Open the stream in a DASH player:

```
http://localhost:8080/dash.mpd
```

## JSON spec mode

Use `-spec` to insert one or more ads at arbitrary timeline points in the program.

```sh
ads/cmd/stitch/bin/stitch -spec ads/cmd/stitch/sample_spec.json
```

Spec fields:

- `output_path` (optional): output MP4 path. Falls back to `-out` if omitted.
- `program_dir` (required): carved program directory containing `vinit/ainit` and chunks.
- `program2_dir` (optional): second program directory appended after the first program timeline.
- `program_chunk_sec` (required): program chunk duration in seconds. Used to map insertion time to chunk index.
- `ads` (required for insertion): ad entries with:
  - `dir`: carved ad directory
  - `insert_at_sec`: insertion time in seconds from start of program timeline

Example:

```json
{
  "output_path": "out/final_with_inserted_ads.mp4",
  "program_dir": "out/prog",
  "program_chunk_sec": 1.6,
  "ads": [
    { "dir": "out/ad_fake", "insert_at_sec": 120.0 },
    { "dir": "out/ad_fake", "insert_at_sec": 360.0 }
  ]
}
```

## Notes

- Insertion is chunk-based, not frame-accurate.
- `program_chunk_sec` should match how the program was carved.
- Ad directories must include both audio and video DASH chunks and init segments.
- DASH mode and MP4 mux mode are mutually exclusive; DASH is chosen when `-dash-dir` or `-serve` is set.
