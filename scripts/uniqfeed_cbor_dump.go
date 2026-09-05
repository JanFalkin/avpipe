//go:build ignore

package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/fxamacker/cbor/v2"
)

var magic = []byte{0xc3, 0xd4, 0x47, 0x4f, 0xe8, 0x86, 0x26, 0x58}

type packet struct {
	ID      int64  `cbor:"id"`
	Payload []byte `cbor:"payload"`
}

func main() {
	path := os.Args[1]
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()

	for i := 0; i < 10; i++ {
		head := make([]byte, len(magic))
		if _, err := io.ReadFull(f, head); err != nil {
			break
		}
		var sz uint64
		binary.Read(f, binary.LittleEndian, &sz)
		buf := make([]byte, sz)
		io.ReadFull(f, buf)
		var pkt packet
		cbor.Unmarshal(buf, &pkt)
		fmt.Printf("packet %d: ID=%d (%.3fs at 90kHz)  payload=%d bytes\n",
			i, pkt.ID, float64(pkt.ID)/90000, len(pkt.Payload))
	}
}
