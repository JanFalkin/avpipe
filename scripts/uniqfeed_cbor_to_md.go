package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/fxamacker/cbor/v2"
)

var magic = []byte{0xc3, 0xd4, 0x47, 0x4f, 0xe8, 0x86, 0x26, 0x58}

type packet struct {
	ID      int64  `cbor:"id"`
	Payload []byte `cbor:"payload"`
}

func main() {
	input := flag.String("input", "", "path to cbor_stream.bin")
	outDir := flag.String("out-dir", "", "output directory for md-000000.bin files")
	count := flag.Int("count", 100, "maximum number of payload files to write")
	startPTS := flag.Int64("start-pts", 0, "skip packets with ID (PTS) below this value")
	flag.Parse()

	if *input == "" || *outDir == "" {
		fmt.Fprintln(os.Stderr, "usage: go run scripts/uniqfeed_cbor_to_md.go -input cbor_stream.bin -out-dir /tmp/md [-count 100]")
		os.Exit(2)
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error creating output directory: %v\n", err)
		os.Exit(1)
	}

	f, err := os.Open(*input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening input: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	written := 0
	for written < *count {
		head := make([]byte, len(magic))
		_, err := io.ReadFull(f, head)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading packet magic: %v\n", err)
			os.Exit(1)
		}
		for i := range magic {
			if head[i] != magic[i] {
				fmt.Fprintf(os.Stderr, "error: invalid packet magic at packet %d\n", written)
				os.Exit(1)
			}
		}

		var packetSize uint64
		if err := binary.Read(f, binary.LittleEndian, &packetSize); err != nil {
			fmt.Fprintf(os.Stderr, "error reading packet size: %v\n", err)
			os.Exit(1)
		}

		buf := make([]byte, packetSize)
		if _, err := io.ReadFull(f, buf); err != nil {
			fmt.Fprintf(os.Stderr, "error reading packet payload: %v\n", err)
			os.Exit(1)
		}

		var pkt packet
		if err := cbor.Unmarshal(buf, &pkt); err != nil {
			fmt.Fprintf(os.Stderr, "error decoding packet %d: %v\n", written, err)
			os.Exit(1)
		}

		// skip packets before the requested start alignment
		if pkt.ID < *startPTS {
			continue
		}

		// always write a file for every packet so frame_index stays in sync;
		// empty payload → 0-byte placeholder (filter treats as no-ad for that frame)
		filename := filepath.Join(*outDir, fmt.Sprintf("md-%06d.bin", written))
		if err := os.WriteFile(filename, pkt.Payload, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing %s: %v\n", filename, err)
			os.Exit(1)
		}
		written++
	}

	fmt.Printf("wrote %d metadata payload files to %s\n", written, *outDir)
}
