#!/usr/bin/env bash

set -euo pipefail

usage() {
    cat <<'EOF'
Usage:
  scripts/uniqfeed-libavfilter-replay.sh [options]

Options:
  -d <dir>   Path to tnt-uniqfeed checkout
             default: ../tnt-uniqfeed relative to this script
  -o <dir>   Output directory
             default: ./test_out/uniqfeed-libavfilter
  -f <path>  FFmpeg binary with uniqfeed filter support
             default: $HOME/.local/bin/ffmpeg, then ffmpeg from PATH
  -n <num>   Number of frames / metadata payloads to process
             default: 100
  -r <WxH>   Scale input to this resolution before uniqfeed (must match project)
             default: queried from project via go run
  -s <id>    Session id for dummy VAST server assets
             default: 1
  --no-mp4   Skip assembling rendered PNGs into an mp4 preview
  -h         Show help

This uses the actual FFmpeg uniqfeed avfilter by:
  1. converting cbor_stream.bin into md-000000.bin files
  2. running ffmpeg -vf uniqfeed=...:metadata_dir=...
  3. writing rendered PNGs for visual inspection
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
DEMO_DIR=$(cd "$SCRIPT_DIR/../../tnt-uniqfeed" 2>/dev/null && pwd || echo "$SCRIPT_DIR/../../tnt-uniqfeed")
OUT_DIR="$SCRIPT_DIR/../test_out/uniqfeed-libavfilter"
FRAME_COUNT=500
SESSION_ID=1
MAKE_MP4=1
FFMPEG_BIN=${FFMPEG_BIN:-}
PROJECT_RESOLUTION=

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
        -n)
            FRAME_COUNT=$2
            shift 2
            ;;
        -r)
            PROJECT_RESOLUTION=$2
            shift 2
            ;;
        -s)
            SESSION_ID=$2
            shift 2
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

PROJECT_DIR="$DEMO_DIR/project"
STREAM_TS="$DEMO_DIR/stream_reader/data/stream.ts"
CBOR_BIN="$DEMO_DIR/stream_reader/data/cbor_stream.bin"
SERVER_DIR="$DEMO_DIR/dummy_vast_server"
METADATA_DIR="$OUT_DIR/metadata"
RENDERED_DIR="$OUT_DIR/rendered"

require_dir "$DEMO_DIR"
require_dir "$PROJECT_DIR"
require_file "$STREAM_TS"
require_file "$CBOR_BIN"
require_dir "$SERVER_DIR"
require_file "$SERVER_DIR/server.py"
require_dir "$SERVER_DIR/session_ads/$SESSION_ID"
require_file "$FFMPEG_BIN"

mkdir -p "$METADATA_DIR" "$RENDERED_DIR"
find "$METADATA_DIR" -maxdepth 1 -name '*.bin' -delete
find "$RENDERED_DIR" -maxdepth 1 \( -name '*.png' -o -name '*.mp4' \) -delete

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

echo "Converting cbor_stream.bin to md-000000.bin payloads"
# detect the stream's first packet PTS so CBOR metadata aligns with the video
stream_info=$("$FFMPEG_BIN" -hide_banner -i "$STREAM_TS" 2>&1 || true)
# each log line is fragmented; the start time (e.g. 441.500000) is the first line
# whose only content (last token) is a decimal with 3+ digits before the point
STREAM_START_SECS=$(echo "$stream_info" | awk '{if ($NF ~ /^[0-9]{3,}\.[0-9]+$/) {print $NF; exit}}')
STREAM_START_PTS=$(echo "${STREAM_START_SECS:-0}" | awk '{printf "%.0f", $1 * 90000}')
echo "Stream start PTS: $STREAM_START_PTS (${STREAM_START_SECS}s)"
pushd "$SCRIPT_DIR/.." >/dev/null
go run ./scripts/uniqfeed_cbor_to_md.go -input "$CBOR_BIN" -out-dir "$METADATA_DIR" -count "$FRAME_COUNT" -start-pts "$STREAM_START_PTS"
popd >/dev/null

if [[ -z "$PROJECT_RESOLUTION" ]]; then
    # probe the project's expected resolution from the renderlib log (1-frame dry run)
    probe_log=$("$FFMPEG_BIN" -hide_banner -i "$STREAM_TS" \
        -vf "uniqfeed=project_path=$PROJECT_DIR:passthrough_on_failure=1" \
        -frames:v 1 -f null /dev/null 2>&1 || true)
    PROJECT_RESOLUTION=$(echo "$probe_log" | grep -oE 'output\.format [0-9]+x[0-9]+' | grep -oE '[0-9]+x[0-9]+' | head -1 || true)
    if [[ -z "$PROJECT_RESOLUTION" ]]; then
        echo "Project resolution detection skipped (not needed for 1920x1080 input)"
    else
        echo "Project render output format: $PROJECT_RESOLUTION (input is native stream resolution)"
    fi
fi

echo "Running FFmpeg uniqfeed avfilter against stream.ts"
"$FFMPEG_BIN" \
    -y \
    -copyts \
    -loglevel warning \
    -i "$STREAM_TS" \
    -map 0:v:0 \
    -an \
    -vf "select='lt(n\,$FRAME_COUNT)',uniqfeed=project_path=$PROJECT_DIR:metadata_dir=$METADATA_DIR:passthrough_on_failure=1" \
    -vsync 0 \
    -f image2 \
    "$RENDERED_DIR/frame_%06d.png"

if [[ "$MAKE_MP4" == 1 ]] && compgen -G "$RENDERED_DIR/frame_*.png" >/dev/null; then
    echo "Assembling preview mp4"
    "$FFMPEG_BIN" \
        -y \
        -framerate 50 \
        -pattern_type glob \
        -i "$RENDERED_DIR/frame_*.png" \
        -c:v libx264 \
        -pix_fmt yuv420p \
        "$RENDERED_DIR/preview.mp4"
fi

echo
echo "Done"
echo "Metadata: $METADATA_DIR"
echo "Rendered: $RENDERED_DIR"
if [[ -f "$RENDERED_DIR/preview.mp4" ]]; then
    echo "Preview:  $RENDERED_DIR/preview.mp4"
fi