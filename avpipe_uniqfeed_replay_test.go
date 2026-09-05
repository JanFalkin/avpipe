package avpipe_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/eluv-io/avpipe"
	"github.com/eluv-io/avpipe/goavpipe"
	"github.com/eluv-io/avpipe/xc"
	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/require"
)

const uniqfeedCBORReplaySpanPTS = int64(2 * 90000)

var uniqfeedCBORMagic = []byte{0xc3, 0xd4, 0x47, 0x4f, 0xe8, 0x86, 0x26, 0x58}

type uniqfeedCBORPacket struct {
	ID      int64  `cbor:"id"`
	Payload []byte `cbor:"payload"`
}

type recordingUniqfeedProvider struct {
	queue *goavpipe.QueuedUniqfeedMetadataProvider

	mu        sync.Mutex
	initCalls int
	getCalls  int
	hits      int
	lastHitID int64
}

func newRecordingUniqfeedProvider() *recordingUniqfeedProvider {
	return &recordingUniqfeedProvider{queue: goavpipe.NewQueuedUniqfeedMetadataProvider()}
}

func (p *recordingUniqfeedProvider) Init(projectPath, metadataDir string) error {
	p.mu.Lock()
	p.initCalls++
	p.mu.Unlock()
	return p.queue.Init(projectPath, metadataDir)
}

func (p *recordingUniqfeedProvider) GetMetadataBlob(frameIndex uint64, streamIndex uint32, renderTID int64) ([]byte, error) {
	blob, err := p.queue.GetMetadataBlob(frameIndex, streamIndex, renderTID)
	p.mu.Lock()
	p.getCalls++
	if len(blob) > 0 {
		p.hits++
		p.lastHitID = renderTID
	}
	p.mu.Unlock()
	return blob, err
}

func (p *recordingUniqfeedProvider) Close() error {
	return p.queue.Close()
}

func (p *recordingUniqfeedProvider) Push(renderTID int64, blob []byte) {
	p.queue.Push(renderTID, blob)
}

func (p *recordingUniqfeedProvider) snapshot() (initCalls, getCalls, hits int, lastHitID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.initCalls, p.getCalls, p.hits, p.lastHitID
}

func TestUniqfeedReplayCBORSmoke(t *testing.T) {
	if os.Getenv("AVPIPE_RUN_UNIQFEED_REPLAY") != "1" {
		t.Skip("set AVPIPE_RUN_UNIQFEED_REPLAY=1 to run the uniqfeed CBOR replay smoke test")
	}

	demoDir := os.Getenv("AVPIPE_UNIQFEED_DEMO_DIR")
	if demoDir == "" {
		demoDir = filepath.Clean(filepath.Join("..", "tnt-uniqfeed"))
	}

	projectPath := filepath.Join(demoDir, "project")
	streamPath := filepath.Join(demoDir, "stream_reader", "data", "stream.ts")
	cborPath := filepath.Join(demoDir, "stream_reader", "data", "cbor_stream.bin")
	serverDir := filepath.Join(demoDir, "dummy_vast_server")

	if !dirExists(projectPath) {
		t.Skipf("uniqfeed project missing: %s", projectPath)
	}
	if !fileExist(streamPath) {
		t.Skipf("uniqfeed sample stream missing: %s", streamPath)
	}
	if !fileExist(cborPath) {
		t.Skipf("uniqfeed sample cbor missing: %s", cborPath)
	}
	if !fileExist(filepath.Join(serverDir, "server.py")) {
		t.Skipf("uniqfeed dummy vast server missing: %s", filepath.Join(serverDir, "server.py"))
	}

	cleanupServer := ensureDummyVASTServer(t, serverDir)
	defer cleanupServer()

	packets := loadUniqfeedCBORPackets(t, cborPath, uniqfeedCBORReplaySpanPTS)
	require.NotEmpty(t, packets)

	provider := newRecordingUniqfeedProvider()
	for _, packet := range packets {
		provider.Push(packet.ID, packet.Payload)
	}

	extractPTS := uniqfeedExtractPTSList(packets, 3)
	require.Len(t, extractPTS, 3)

	outDir := t.TempDir()
	goavpipe.InitIOHandler(&xc.FileInputOpener{URL: streamPath}, &xc.FileOutputOpener{Dir: outDir})

	params := goavpipe.NewXcParams()
	params.Url = streamPath
	params.Format = "image2"
	params.Ecodec = "mjpeg"
	params.XcType = goavpipe.XcExtractImages
	params.Seekable = true
	params.StartTimeTs = 0
	params.DurationTs = -1
	params.VideoTimeBase = 90000
	params.EncWidth = -1
	params.EncHeight = -1
	params.ExtractImagesTs = extractPTS
	params.UniqfeedProjectPath = projectPath
	params.UniqfeedPassthroughOnFailure = true
	params.UniqfeedMetadataProvider = provider

	err := avpipe.Xc(params)
	require.NoError(t, err)

	initCalls, getCalls, hits, lastHitID := provider.snapshot()
	require.Greater(t, initCalls, 0, "uniqfeed provider should be initialized")
	require.Greater(t, getCalls, 0, "uniqfeed provider should be queried during transcode")
	require.Greater(t, hits, 0, "uniqfeed provider should return at least one metadata blob matched by render_tid")
	require.Greater(t, lastHitID, int64(0))

	for _, pts := range extractPTS {
		require.FileExists(t, filepath.Join(outDir, fmt.Sprintf("%d.jpeg", pts)))
	}
}

func ensureDummyVASTServer(t *testing.T, serverDir string) func() {
	t.Helper()

	if isHTTPReachable("http://localhost:8080/vast.xml?session=1") {
		return func() {}
	}

	cmd := exec.Command("python3", "server.py")
	cmd.Dir = serverDir
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	require.NoError(t, cmd.Start())

	require.Eventually(t, func() bool {
		return isHTTPReachable("http://localhost:8080/vast.xml?session=1")
	}, 5*time.Second, 100*time.Millisecond, "dummy vast server failed to start: %s", output.String())

	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

func isHTTPReachable(url string) bool {
	resp, err := http.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500
}

func dirExists(dir string) bool {
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return false
	}
	return err == nil && info.IsDir()
}

func loadUniqfeedCBORPackets(t *testing.T, cborPath string, maxSpanPTS int64) []uniqfeedCBORPacket {
	t.Helper()

	file, err := os.Open(cborPath)
	require.NoError(t, err)
	defer func() { _ = file.Close() }()

	packets := make([]uniqfeedCBORPacket, 0, 128)
	var firstID int64
	haveFirstID := false

	for {
		magic := make([]byte, len(uniqfeedCBORMagic))
		_, err := io.ReadFull(file, magic)
		if err == io.EOF {
			break
		}
		if err == io.ErrUnexpectedEOF {
			break
		}
		require.NoError(t, err)
		require.Equal(t, uniqfeedCBORMagic, magic)

		var packetSize uint64
		require.NoError(t, binary.Read(file, binary.LittleEndian, &packetSize))

		payload := make([]byte, packetSize)
		_, err = io.ReadFull(file, payload)
		require.NoError(t, err)

		var packet uniqfeedCBORPacket
		require.NoError(t, cbor.Unmarshal(payload, &packet))
		require.NotEmpty(t, packet.Payload)

		if !haveFirstID {
			firstID = packet.ID
			haveFirstID = true
		}

		if packet.ID-firstID > maxSpanPTS {
			break
		}

		packets = append(packets, packet)
	}

	require.NotEmpty(t, packets, fmt.Sprintf("no packets loaded from %s", cborPath))
	return packets
}

func uniqfeedExtractPTSList(packets []uniqfeedCBORPacket, count int) []int64 {
	pts := make([]int64, 0, count)
	seen := make(map[int64]struct{}, count)
	for _, packet := range packets {
		if _, ok := seen[packet.ID]; ok {
			continue
		}
		seen[packet.ID] = struct{}{}
		pts = append(pts, packet.ID)
		if len(pts) == count {
			break
		}
	}
	return pts
}

// TestUniqfeedReplayCBORVideo transcodes a MPEG-TS stream with uniqfeed ads
// through the full avpipe.Xc() pipeline. It loads ALL CBOR packets into the
// QueuedUniqfeedMetadataProvider so every video frame whose PTS matches a
// CBOR packet ID will have ads composited by the renderlib + VAST server.
//
// Enable with: AVPIPE_RUN_UNIQFEED_REPLAY=1
func TestUniqfeedReplayCBORVideo(t *testing.T) {
	if os.Getenv("AVPIPE_RUN_UNIQFEED_REPLAY") != "1" {
		t.Skip("set AVPIPE_RUN_UNIQFEED_REPLAY=1 to run the uniqfeed CBOR video replay test")
	}

	demoDir := os.Getenv("AVPIPE_UNIQFEED_DEMO_DIR")
	if demoDir == "" {
		demoDir = filepath.Clean(filepath.Join("..", "tnt-uniqfeed"))
	}

	projectPath := filepath.Join(demoDir, "project")
	streamPath := filepath.Join(demoDir, "stream_reader", "data", "stream.ts")
	cborPath := filepath.Join(demoDir, "stream_reader", "data", "cbor_stream.bin")
	serverDir := filepath.Join(demoDir, "dummy_vast_server")

	for _, p := range []string{projectPath, serverDir} {
		if !dirExists(p) {
			t.Skipf("missing: %s", p)
		}
	}
	for _, f := range []string{streamPath, cborPath} {
		if !fileExist(f) {
			t.Skipf("missing: %s", f)
		}
	}

	cleanupServer := ensureDummyVASTServer(t, serverDir)
	defer cleanupServer()

	// Load all CBOR packets — the provider matches by renderTID (PTS) so every
	// frame that aligns with a CBOR packet will have ads composited.
	packets, err := loadAllUniqfeedCBORPackets(t, cborPath)
	require.NoError(t, err)
	require.NotEmpty(t, packets)
	t.Logf("loaded %d CBOR packets (PTS %d – %d)", len(packets), packets[0].ID, packets[len(packets)-1].ID)

	provider := newRecordingUniqfeedProvider()
	for _, pkt := range packets {
		if len(pkt.Payload) > 0 {
			provider.Push(pkt.ID, pkt.Payload)
		}
	}

	outDir := filepath.Join("test_out", t.Name())
	require.NoError(t, os.MkdirAll(outDir, 0o755))

	goavpipe.InitIOHandler(
		&xc.FileInputOpener{URL: streamPath},
		&xc.FileOutputOpener{Dir: outDir},
	)

	params := goavpipe.NewXcParams()
	params.Url                          = streamPath
	params.Format                       = "hls"
	params.Ecodec                       = "libx264"
	params.XcType                       = goavpipe.XcVideo
	params.Seekable                     = true
	params.DurationTs                   = -1
	params.VideoBitrate                 = 4000000
	params.VideoSegDurationTs           = 180000 // 2s at 90kHz
	params.StartSegmentStr              = "1"
	params.EncWidth                     = 1920
	params.EncHeight                    = 1080
	params.UniqfeedProjectPath          = projectPath
	params.UniqfeedPassthroughOnFailure = true
	params.UniqfeedViewerProfile        = "session=1"
	params.UniqfeedMetadataProvider     = provider

	err = avpipe.Xc(params)
	require.NoError(t, err)

	_, getCalls, hits, _ := provider.snapshot()
	t.Logf("provider: getCalls=%d hits=%d", getCalls, hits)
	require.Greater(t, getCalls, 0, "provider should be queried for every video frame")
	require.Greater(t, hits, 0, "at least one frame should have matching CBOR metadata")
}

// loadAllUniqfeedCBORPackets reads the entire CBOR stream without any PTS limit.
func loadAllUniqfeedCBORPackets(t *testing.T, cborPath string) ([]uniqfeedCBORPacket, error) {
	t.Helper()
	file, err := os.Open(cborPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	var packets []uniqfeedCBORPacket
	for {
		magic := make([]byte, len(uniqfeedCBORMagic))
		if _, err := io.ReadFull(file, magic); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, err
		}
		var packetSize uint64
		if err := binary.Read(file, binary.LittleEndian, &packetSize); err != nil {
			return nil, err
		}
		payload := make([]byte, packetSize)
		if _, err := io.ReadFull(file, payload); err != nil {
			return nil, err
		}
		var packet uniqfeedCBORPacket
		if err := cbor.Unmarshal(payload, &packet); err != nil {
			return nil, err
		}
		packets = append(packets, packet)
	}
	return packets, nil
}
