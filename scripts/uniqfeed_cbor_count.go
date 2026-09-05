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
	f, _ := os.Open(os.Args[1])
	defer f.Close()
	var n int
	var first, last int64
	for {
		head := make([]byte, 8)
		if _, err := io.ReadFull(f, head); err != nil { break }
		var sz uint64
		binary.Read(f, binary.LittleEndian, &sz)
		buf := make([]byte, sz)
		io.ReadFull(f, buf)
		var pkt packet
		cbor.Unmarshal(buf, &pkt)
		if n == 0 { first = pkt.ID }
		last = pkt.ID
		n++
	}
	fmt.Printf("total packets: %d\nfirst PTS: %d (%.3fs)\nlast  PTS: %d (%.3fs)\n",
		n, first, float64(first)/90000, last, float64(last)/90000)
}
