#!/usr/bin/env bash
# Replay uniqfeed ads on a recorded stream using read_stream (same path as production).
# Extracts frames from stream.ts with PTS-based filenames, then runs read_stream
# which matches each frame to its CBOR metadata by exact PTS and renders ads.

set -euo pipefail

usage() {
    cat <<'EOF'
Usage:
  scripts/uniqfeed-stream-replay.sh [options]

Options:
  -d <dir>   Path to tnt-uniqfeed checkout
             default: ../tnt-uniqfeed relative to this script
  -o <dir>   Output directory
             default: ./test_out/uniqfeed-replay
  -f <path>  FFmpeg binary (uniqfeed-patched build)
             default: $HOME/.local/bin/ffmpeg, then ffmpeg from PATH
  -n <num>   Number of frames to extract and process
             default: 600
  -s <id>    Session id for dummy VAST server assets
             default: 1
  --no-mp4   Skip assembling rendered PNGs into a preview mp4
  -h         Show help

How it works:
  1. Extracts N frames from stream.ts as frame_<PTS>.png (PTS in 90kHz ticks)
  2. Starts the dummy VAST server if not already running
  3. Runs read_stream which matches each frame to its CBOR metadata by PTS
     and renders ads using the uniqFEED renderlib + VAST server
  4. Assembles rendered PNGs into a preview mp4
EOF
}

require_file() { [[ -f "$1" ]] || { echo "error: file not found: $1" >&2; exit 1; }; }
require_dir()  { [[ -d "$1" ]] || { echo "error: directory not found: $1" >&2; exit 1; }; }

http_ready() {
    curl --silent --show-error --fail "$1" >/dev/null 2>&1
}

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
DEMO_DIR=$(cd "$SCRIPT_DIR/../../tnt-uniqfeed" 2>/dev/null && pwd || echo "$SCRIPT_DIR/../../tnt-uniqfeed")
OUT_DIR="$SCRIPT_DIR/../test_out/uniqfeed-replay"
FRAME_COUNT=600
SESSION_ID=1
MAKE_MP4=1
FFMPEG_BIN=${FFMPEG_BIN:-}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -d) DEMO_DIR=$2;    shift 2 ;;
        -o) OUT_DIR=$2;     shift 2 ;;
        -f) FFMPEG_BIN=$2;  shift 2 ;;
        -n) FRAME_COUNT=$2; shift 2 ;;
        -s) SESSION_ID=$2;  shift 2 ;;
        --no-mp4) MAKE_MP4=0; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "error: unknown option: $1" >&2; usage >&2; exit 1 ;;
    esac
done

if [[ -z "$FFMPEG_BIN" ]]; then
    if [[ -x "$HOME/.local/bin/ffmpeg" ]]; then
        FFMPEG_BIN="$HOME/.local/bin/ffmpeg"
    else
        FFMPEG_BIN=$(command -v ffmpeg)
    fi
fi

PROJECT_DIR="$DEMO_DIR/project"
STREAM_TS="$DEMO_DIR/stream_reader/data/stream.ts"
CBOR_BIN="$DEMO_DIR/stream_reader/data/cbor_stream.bin"
READ_STREAM="$DEMO_DIR/stream_reader/read_stream"
SERVER_DIR="$DEMO_DIR/dummy_vast_server"
FRAMES_DIR="$OUT_DIR/frames"
RENDERED_DIR="$OUT_DIR/rendered"

require_dir "$DEMO_DIR"
require_dir "$PROJECT_DIR"
require_file "$STREAM_TS"
require_file "$CBOR_BIN"
require_file "$READ_STREAM"
require_dir "$SERVER_DIR"
require_file "$SERVER_DIR/server.py"
require_dir "$SERVER_DIR/session_ads/$SESSION_ID"

mkdir -p "$FRAMES_DIR" "$RENDERED_DIR"
find "$FRAMES_DIR"   -maxdepth 1 -name '*.png' -delete
find "$RENDERED_DIR" -maxdepth 1 \( -name '*.png' -o -name '*.mp4' \) -delete

# Start VAST server if not already running
SERVER_STARTED=0
if ! http_ready "http://localhost:8080/vast.xml?session=$SESSION_ID"; then
    pushd "$SERVER_DIR" >/dev/null
    python3 server.py >"$OUT_DIR/vast-server.log" 2>&1 &
    SERVER_PID=$!
    popd >/dev/null
    SERVER_STARTED=1

    for _ in $(seq 1 50); do
        if http_ready "http://localhost:8080/vast.xml?session=$SESSION_ID"; then break; fi
        perl -e 'select undef, undef, undef, 0.1'
    done

    if ! http_ready "http://localhost:8080/vast.xml?session=$SESSION_ID"; then
        echo "error: dummy VAST server did not start; see $OUT_DIR/vast-server.log" >&2
        exit 1
    fi
fi

cleanup() {
    if [[ "$SERVER_STARTED" == 1 ]]; then
        kill "$SERVER_PID" >/dev/null 2>&1 || true
        wait "$SERVER_PID" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

export LD_LIBRARY_PATH="$HOME/.local/lib:$HOME/.local/lib/uf:$HOME/.local/lib/3rdparty:${LD_LIBRARY_PATH:-}"

# Extract frames and capture their PTS values from the showinfo filter.
# read_stream expects files named frame_<PTS>.png (PTS in 90kHz ticks).
echo "Extracting $FRAME_COUNT frames from stream.ts..."
pts_file="$OUT_DIR/pts_list.txt"
"$FFMPEG_BIN" -y -loglevel warning \
    -i "$STREAM_TS" -map 0:v:0 -an \
    -vf "select='lt(n\,$FRAME_COUNT)',showinfo" \
    -vsync 0 -f image2 "$FRAMES_DIR/tmp_%06d.png" 2>&1 | \
    grep -oP 'pts:\s*\K[0-9]+' > "$pts_file"

frame_count_actual=$(wc -l < "$pts_file")
echo "Extracted $frame_count_actual frames"

# Rename tmp_NNNNNN.png → frame_<PTS>.png so read_stream can match by PTS
echo "Renaming frames to PTS-based filenames..."
paste <(ls "$FRAMES_DIR"/tmp_*.png | sort) "$pts_file" | \
while IFS=$'\t' read -r src pts; do
    mv "$src" "$FRAMES_DIR/frame_${pts}.png"
done
echo "Renamed $(ls "$FRAMES_DIR"/frame_*.png | wc -l) frames"

# Run read_stream: matches each frame to CBOR metadata by exact PTS, renders ads
echo "Running read_stream..."
"$READ_STREAM" "$PROJECT_DIR" "$CBOR_BIN" "$FRAMES_DIR" "$RENDERED_DIR" "session=$SESSION_ID"

rendered_count=$(ls "$RENDERED_DIR"/*.png 2>/dev/null | wc -l)
echo "Rendered $rendered_count frames to $RENDERED_DIR"

if [[ "$MAKE_MP4" == 1 ]] && compgen -G "$RENDERED_DIR/*.png" >/dev/null; then
    echo "Assembling preview mp4..."
    "$FFMPEG_BIN" -y -loglevel warning \
        -framerate 50 \
        -pattern_type glob \
        -i "$RENDERED_DIR/*.png" \
        -c:v libx264 -pix_fmt yuv420p \
        "$RENDERED_DIR/preview.mp4"
fi

echo
echo "Done"
echo "Frames:   $FRAMES_DIR"
echo "Rendered: $RENDERED_DIR"
[[ -f "$RENDERED_DIR/preview.mp4" ]] && echo "Preview:  $RENDERED_DIR/preview.mp4"
