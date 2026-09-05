//go:build ignore

// Replay uniqfeed ads on a recorded stream through the avpipe pipeline.
// Loads ALL CBOR packets into QueuedUniqfeedMetadataProvider and runs avpipe.Xc()
// to produce a full MP4 with ads composited by renderTID (same path as production).
//
// Usage:
//   go run scripts/uniqfeed_avpipe_replay.go \
//     -project  /path/to/tnt-uniqfeed/project \
//     -cbor     /path/to/cbor_stream.bin \
//     -stream   /path/to/stream.ts \
//     -out      /tmp/replay_out \
//     [-frames  N]         # max frames (default: all)
//     [-session session=1] # VAST viewer profile string

package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/eluv-io/avpipe"
	"github.com/eluv-io/avpipe/goavpipe"
	"github.com/eluv-io/avpipe/xc"
	"github.com/fxamacker/cbor/v2"
)

var magic = []byte{0xc3, 0xd4, 0x47, 0x4f, 0xe8, 0x86, 0x26, 0x58}

type cborPacket struct {
	ID      int64  `cbor:"id"`
	Payload []byte `cbor:"payload"`
}

func loadAllCBOR(path string, maxFrames int) ([]cborPacket, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var packets []cborPacket
	for maxFrames <= 0 || len(packets) < maxFrames {
		head := make([]byte, len(magic))
		if _, err := io.ReadFull(f, head); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, fmt.Errorf("reading magic: %w", err)
		}
		for i := range magic {
			if head[i] != magic[i] {
				return nil, fmt.Errorf("invalid magic at packet %d", len(packets))
			}
		}
		var sz uint64
		if err := binary.Read(f, binary.LittleEndian, &sz); err != nil {
			return nil, fmt.Errorf("reading size: %w", err)
		}
		buf := make([]byte, sz)
		if _, err := io.ReadFull(f, buf); err != nil {
			return nil, fmt.Errorf("reading payload: %w", err)
		}
		var pkt cborPacket
		if err := cbor.Unmarshal(buf, &pkt); err != nil {
			return nil, fmt.Errorf("decoding packet %d: %w", len(packets), err)
		}
		packets = append(packets, pkt)
	}
	return packets, nil
}

func main() {
	projectPath := flag.String("project", "", "path to uniqFEED project directory")
	cborPath := flag.String("cbor", "", "path to cbor_stream.bin")
	streamPath := flag.String("stream", "", "path to stream.ts")
	outDir := flag.String("out", "", "output directory for rendered mp4 segments")
	maxFrames := flag.Int("frames", 0, "max CBOR packets to load (0 = all)")
	session := flag.String("session", "session=1", "VAST viewer profile string")
	flag.Parse()

	if *projectPath == "" || *cborPath == "" || *streamPath == "" || *outDir == "" {
		fmt.Fprintln(os.Stderr, "usage: go run scripts/uniqfeed_avpipe_replay.go -project <p> -cbor <c> -stream <s> -out <o>")
		os.Exit(2)
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error creating output dir: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Loading CBOR from %s...\n", *cborPath)
	packets, err := loadAllCBOR(*cborPath, *maxFrames)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading CBOR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Loaded %d CBOR packets (PTS range: %d - %d)\n",
		len(packets), packets[0].ID, packets[len(packets)-1].ID)

	provider := goavpipe.NewQueuedUniqfeedMetadataProvider()
	for _, pkt := range packets {
		if len(pkt.Payload) > 0 {
			provider.Push(pkt.ID, pkt.Payload)
		}
	}
	fmt.Printf("Pushed %d non-empty packets to provider\n", len(packets))

	outFile := filepath.Join(*outDir, "replay.mp4")
	goavpipe.InitIOHandler(
		&xc.FileInputOpener{URL: *streamPath},
		&xc.FileOutputOpener{Dir: *outDir},
	)

	params := goavpipe.NewXcParams()
	params.Url = *streamPath
	params.Format = "hls"
	params.Ecodec = "libx264"
	params.XcType = goavpipe.XcVideo
	params.Seekable = true
	params.DurationTs = -1
	params.VideoBitrate = 4000000
	params.VideoSegDurationTs = 180000 // 2s segments at 90kHz
	params.StartSegmentStr = "1"
	params.EncWidth = 1920
	params.EncHeight = 1080
	params.UniqfeedProjectPath = *projectPath
	params.UniqfeedPassthroughOnFailure = true
	params.UniqfeedViewerProfile = *session
	params.UniqfeedMetadataProvider = provider
	// viewer profile (session) is embedded in CBOR payload targeting data
	_ = *session

	fmt.Printf("Starting avpipe transcode: %s -> %s\n", *streamPath, outFile)
	if err := avpipe.Xc(params); err != nil {
		fmt.Fprintf(os.Stderr, "avpipe.Xc failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Done")
}
