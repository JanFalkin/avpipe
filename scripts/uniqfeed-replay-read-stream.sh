#!/usr/bin/env bash

set -euo pipefail

usage() {
    cat <<'EOF'
Usage:
  scripts/uniqfeed-replay-read-stream.sh [options]

Options:
  -d <dir>   Path to tnt-uniqfeed checkout
             default: ../tnt-uniqfeed relative to this script
  -o <dir>   Output directory for extracted and rendered PNGs
             default: ./test_out/uniqfeed-read-stream
  -f <path>  FFmpeg binary to use for frame extraction / optional mp4 assembly
             default: $HOME/.local/bin/ffmpeg, then ffmpeg from PATH
  -t <sec>   Duration in seconds to extract from stream.ts
             default: 2
  -s <id>    Session id for dummy VAST server assets
             default: 1
  --keep-frames
             Keep previously extracted frame PNGs
  --no-mp4   Skip assembling feed-0 PNGs into an mp4 preview
  -h         Show help

This mirrors the sibling stream_reader/read_stream workflow using:
  - stream_reader/data/stream.ts
  - stream_reader/data/cbor_stream.bin
  - project/

Outputs:
  <outdir>/frames/frame_<pts>.png
  <outdir>/rendered/feed-<n>-<pts>.png
  <outdir>/rendered/feed-0.mp4     (unless --no-mp4)
EOF
}

require_file() {
    local path=$1
    [[ -f "$path" ]] || {
        echo "error: file not found: $path" >&2
        exit 1
    }
}

require_dir() {
    local path=$1
    [[ -d "$path" ]] || {
        echo "error: directory not found: $path" >&2
        exit 1
    }
}

http_ready() {
    local url=$1
    curl --silent --show-error --fail "$url" >/dev/null 2>&1
}

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
DEMO_DIR=$(cd "$SCRIPT_DIR/../../tnt-uniqfeed" && pwd)
OUT_DIR="$SCRIPT_DIR/../test_out/uniqfeed-read-stream"
DURATION=2
SESSION_ID=1
KEEP_FRAMES=0
MAKE_MP4=1
FFMPEG_BIN=${FFMPEG_BIN:-}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -d)
            DEMO_DIR=$2
            shift 2
            ;;
        -o)
            OUT_DIR=$2
            shift 2
            ;;
        -f)
            FFMPEG_BIN=$2
            shift 2
            ;;
        -t)
            DURATION=$2
            shift 2
            ;;
        -s)
            SESSION_ID=$2
            shift 2
            ;;
        --keep-frames)
            KEEP_FRAMES=1
            shift
            ;;
        --no-mp4)
            MAKE_MP4=0
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "error: unknown option: $1" >&2
            usage >&2
            exit 1
            ;;
    esac
done

if [[ -z "$FFMPEG_BIN" ]]; then
    if [[ -x "$HOME/.local/bin/ffmpeg" ]]; then
        FFMPEG_BIN="$HOME/.local/bin/ffmpeg"
    else
        FFMPEG_BIN=$(command -v ffmpeg)
    fi
fi

READ_STREAM_BIN="$DEMO_DIR/stream_reader/read_stream"
PROJECT_DIR="$DEMO_DIR/project"
STREAM_TS="$DEMO_DIR/stream_reader/data/stream.ts"
CBOR_BIN="$DEMO_DIR/stream_reader/data/cbor_stream.bin"
SERVER_DIR="$DEMO_DIR/dummy_vast_server"
FRAME_DIR="$OUT_DIR/frames"
RENDERED_DIR="$OUT_DIR/rendered"

require_dir "$DEMO_DIR"
require_file "$READ_STREAM_BIN"
require_dir "$PROJECT_DIR"
require_file "$STREAM_TS"
require_file "$CBOR_BIN"
require_dir "$SERVER_DIR"
require_file "$SERVER_DIR/server.py"
require_dir "$SERVER_DIR/session_ads/$SESSION_ID"
require_file "$FFMPEG_BIN"

mkdir -p "$FRAME_DIR" "$RENDERED_DIR"

if [[ "$KEEP_FRAMES" != 1 ]]; then
    rm -f "$FRAME_DIR"/*.png
fi
rm -f "$RENDERED_DIR"/*.png "$RENDERED_DIR"/*.mp4

SERVER_STARTED=0
if ! http_ready "http://localhost:8080/vast.xml?session=$SESSION_ID"; then
    pushd "$SERVER_DIR" >/dev/null
    python3 server.py >"$OUT_DIR/vast-server.log" 2>&1 &
    SERVER_PID=$!
    popd >/dev/null
    SERVER_STARTED=1

    for _ in $(seq 1 50); do
        if http_ready "http://localhost:8080/vast.xml?session=$SESSION_ID"; then
            break
        fi
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

echo "Extracting frame_<pts>.png from stream.ts"
"$FFMPEG_BIN" \
    -y \
    -copyts \
    -i "$STREAM_TS" \
    -map 0:v:0 \
    -vsync 0 \
    -t "$DURATION" \
    -frame_pts 1 \
    "$FRAME_DIR/frame_%d.png"

echo "Running read_stream against extracted frames and cbor_stream.bin"
pushd "$DEMO_DIR/stream_reader" >/dev/null
./read_stream "$PROJECT_DIR" "$CBOR_BIN" "$FRAME_DIR" "$RENDERED_DIR" "session=$SESSION_ID"
popd >/dev/null

if [[ "$MAKE_MP4" == 1 ]] && compgen -G "$RENDERED_DIR/feed-0-*.png" >/dev/null; then
    echo "Assembling feed-0 preview mp4"
    "$FFMPEG_BIN" \
        -y \
        -framerate 50 \
        -pattern_type glob \
        -i "$RENDERED_DIR/feed-0-*.png" \
        -c:v libx264 \
        -pix_fmt yuv420p \
        "$RENDERED_DIR/feed-0.mp4"
fi

echo
echo "Done"
echo "Frames:    $FRAME_DIR"
echo "Rendered:  $RENDERED_DIR"
if [[ -f "$RENDERED_DIR/feed-0.mp4" ]]; then
    echo "Preview:   $RENDERED_DIR/feed-0.mp4"
fi