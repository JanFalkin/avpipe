package ads

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/eluv-io/avpipe"
	"github.com/eluv-io/avpipe/goavpipe"
)

// CarveParams controls how an MP4 is carved into DASH m4s chunks.
type CarveParams struct {
	// Output directory; will be created if absent.
	OutDir string
	// Video segment duration in timebase units (e.g. 48000 @ 24000 tb ≈ 2 s).
	VideoSegDurationTs int64
	// Audio segment duration in timebase units (e.g. 96000 @ 48000 sr ≈ 2 s).
	AudioSegDurationTs int64
	// ForceKeyInt forces an IDR frame every N frames, keeping segment boundaries clean.
	ForceKeyInt int32
	// VideoBitrate in bits/s (0 = use source bitrate via CRF).
	VideoBitrate int32
	// AudioBitrate in bits/s.
	AudioBitrate int32
	// EncHeight / EncWidth: -1 means keep source dimensions.
	EncHeight int32
	EncWidth  int32
	// VideoFilter is an optional ffmpeg video filter descriptor (e.g. "fps=30").
	// Use this to normalise frame rate across all sources before muxing.
	VideoFilter string
}

// DefaultCarveParams returns sensible defaults for a 24 fps source.
func DefaultCarveParams(outDir string) CarveParams {
	return CarveParams{
		OutDir:             outDir,
		VideoSegDurationTs: 48000, // 2 s @ 24000 timebase
		AudioSegDurationTs: 96000, // 2 s @ 48000 sample rate
		ForceKeyInt:        48,
		VideoBitrate:       2560000,
		AudioBitrate:       128000,
		EncHeight:          -1,
		EncWidth:           -1,
	}
}

// fileOutputOpener writes DASH m4s segments to disk under a directory.
// It implements goavpipe.OutputOpener (used by avpipe.Xc for transcode output).
type fileOutputOpener struct{ dir string }

func (o *fileOutputOpener) Open(_ int64, _ int64, streamIndex, segIndex int, pts int64, outType goavpipe.AVType) (goavpipe.OutputHandler, error) {
	_ = pts
	var name string
	switch outType {
	case goavpipe.DASHVideoInit:
		name = fmt.Sprintf("vinit-stream%d.m4s", streamIndex)
	case goavpipe.DASHAudioInit:
		name = fmt.Sprintf("ainit-stream%d.m4s", streamIndex)
	case goavpipe.DASHVideoSegment:
		name = fmt.Sprintf("vchunk-stream%d-%05d.m4s", streamIndex, segIndex)
	case goavpipe.DASHAudioSegment:
		name = fmt.Sprintf("achunk-stream%d-%05d.m4s", streamIndex, segIndex)
	case goavpipe.DASHManifest:
		name = "dash.mpd"
	default:
		name = fmt.Sprintf("segment-%d-%d.bin", streamIndex, segIndex)
	}
	path := name
	if o.dir != "" {
		path = o.dir + "/" + name
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	return &localOutput{f: f}, nil
}

// Carve transcodes inputMP4 into DASH m4s chunks (vinit, vchunk, ainit, achunk)
// under cp.OutDir, using the given CarveParams. The output files are then
// ready to be passed to StitchWithAd via collectAV.
func Carve(inputMP4 string, cp CarveParams) error {
	if err := os.MkdirAll(cp.OutDir, 0755); err != nil {
		return fmt.Errorf("create output dir %s: %w", cp.OutDir, err)
	}

	base := &goavpipe.XcParams{
		Url:             inputMP4,
		Format:          "dash",
		Seekable:        true,
		StartSegmentStr: "1",
		DurationTs:      -1,
		StreamId:        -1,
		ForceKeyInt:     cp.ForceKeyInt,
		VideoBitrate:    cp.VideoBitrate,
		AudioBitrate:    cp.AudioBitrate,
		EncHeight:       cp.EncHeight,
		EncWidth:        cp.EncWidth,
	}

	// --- video pass ---
	vparams := *base
	vparams.XcType = goavpipe.XcVideo
	vparams.VideoSegDurationTs = cp.VideoSegDurationTs
	vparams.Ecodec = "libx264"
	vparams.FilterDescriptor = cp.VideoFilter
	goavpipe.InitUrlIOHandler(inputMP4, &localInputOpener{}, &fileOutputOpener{dir: cp.OutDir})
	if err := avpipe.Xc(&vparams); err != nil {
		return fmt.Errorf("video carve of %s: %w", inputMP4, err)
	}

	// --- audio pass ---
	aparams := *base
	aparams.XcType = goavpipe.XcAudio
	aparams.AudioSegDurationTs = cp.AudioSegDurationTs
	aparams.Ecodec2 = "aac"
	aparams.SampleRate = 48000
	// Keep a stable channel layout across all carved inputs so stitched outputs
	// can be re-carved without "changing audio frame properties" failures.
	// FFmpeg layout mask 3 == stereo.
	aparams.ChannelLayout = 3
	goavpipe.InitUrlIOHandler(inputMP4, &localInputOpener{}, &fileOutputOpener{dir: cp.OutDir})
	if err := avpipe.Xc(&aparams); err != nil {
		return fmt.Errorf("audio carve of %s: %w", inputMP4, err)
	}
	// A video-only source produces no audio init segment — that is fine.
	if _, err := os.Stat(cp.OutDir + "/ainit-stream0.m4s"); os.IsNotExist(err) {
		fmt.Printf("note: %s has no audio track — generating silent audio\n", inputMP4)
		if err := generateSilenceAudio(cp.OutDir, cp.AudioSegDurationTs); err != nil {
			return fmt.Errorf("silence generation for %s: %w", inputMP4, err)
		}
	}

	return nil
}

// OverwriteDASHManifest rewrites the dash.mpd in outDir after a Carve() call.
// Avpipe runs a video pass then an audio pass, each generating their own
// single-stream manifest under the same "dash.mpd" name — the audio pass
// overwrites the video one, leaving only an audio AdaptationSet. Additionally,
// the avpipe template references "init-stream0.m4s" while our fileOutputOpener
// creates "vinit-stream0.m4s" / "ainit-stream0.m4s", so the URLs wouldn't match.
// This function scans the real segments on disk and writes a correct dual-stream MPD.
func OverwriteDASHManifest(outDir string, cp CarveParams) error {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return fmt.Errorf("read dash dir %s: %w", outDir, err)
	}

	var vChunks, aChunks []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, "vchunk-stream0-") && strings.HasSuffix(n, ".m4s") {
			vChunks = append(vChunks, n)
		} else if strings.HasPrefix(n, "achunk-stream0-") && strings.HasSuffix(n, ".m4s") {
			aChunks = append(aChunks, n)
		}
	}
	sort.Strings(vChunks)
	sort.Strings(aChunks)

	if len(vChunks) == 0 {
		return fmt.Errorf("no video chunks found in %s", outDir)
	}

	// Derive total duration from audio chunk count and segment duration.
	totalSec := float64(len(aChunks)) * float64(cp.AudioSegDurationTs) / 48000.0
	if totalSec <= 0 {
		// Fallback: estimate from video chunks assuming ~2s each.
		totalSec = float64(len(vChunks)) * 2.0
	}

	// Segment duration in ms (timescale=1000) for video.
	// AudioSegDurationTs / 48000 * 1000 gives ms when audio is at 48 kHz.
	videoSegMs := int64(float64(cp.AudioSegDurationTs) / 48.0) // ≈ 2000

	type trackInfo struct {
		NumChunks   int
		InitSeg     string
		MediaTmpl   string
		Timescale   int64
		SegDuration int64 // in Timescale units
	}

	video := trackInfo{
		NumChunks:   len(vChunks),
		InitSeg:     "vinit-stream0.m4s",
		MediaTmpl:   "vchunk-stream0-$Number%05d$.m4s",
		Timescale:   1000,
		SegDuration: videoSegMs,
	}
	audio := trackInfo{
		NumChunks:   len(aChunks),
		InitSeg:     "ainit-stream0.m4s",
		MediaTmpl:   "achunk-stream0-$Number%05d$.m4s",
		Timescale:   48000,
		SegDuration: cp.AudioSegDurationTs,
	}

	data := struct {
		TotalSec string
		Video    trackInfo
		Audio    trackInfo
	}{
		TotalSec: fmt.Sprintf("PT%.3fS", totalSec),
		Video:    video,
		Audio:    audio,
	}

	const tmpl = `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"
     xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
     xsi:schemaLocation="urn:mpeg:dash:schema:mpd:2011 DASH-MPD.xsd"
     type="static"
     mediaPresentationDuration="{{.TotalSec}}"
     minBufferTime="PT4S"
     profiles="urn:mpeg:dash:profile:isoff-live:2011">
  <Period>
    <AdaptationSet id="0" contentType="video" startWithSAP="1" segmentAlignment="true" bitstreamSwitching="true">
      <Representation id="0" mimeType="video/mp4" codecs="avc1.640028" bandwidth="2560000">
        <SegmentTemplate timescale="{{.Video.Timescale}}"
                         initialization="{{.Video.InitSeg}}"
                         media="{{.Video.MediaTmpl}}"
                         startNumber="1"
                         duration="{{.Video.SegDuration}}">
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
    <AdaptationSet id="1" contentType="audio" startWithSAP="1" segmentAlignment="true" bitstreamSwitching="true">
      <Representation id="1" mimeType="audio/mp4" codecs="mp4a.40.2" audioSamplingRate="48000" bandwidth="128000">
        <AudioChannelConfiguration schemeIdUri="urn:mpeg:dash:23003:3:audio_channel_configuration:2011" value="2"/>
        <SegmentTemplate timescale="{{.Audio.Timescale}}"
                         initialization="{{.Audio.InitSeg}}"
                         media="{{.Audio.MediaTmpl}}"
                         startNumber="1"
                         duration="{{.Audio.SegDuration}}">
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>
`
	t, err := template.New("mpd").Parse(tmpl)
	if err != nil {
		return fmt.Errorf("parse MPD template: %w", err)
	}

	mpdPath := filepath.Join(outDir, "dash.mpd")
	f, err := os.OpenFile(mpdPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("create dash.mpd: %w", err)
	}
	if werr := t.Execute(f, data); werr != nil {
		_ = f.Close()
		return fmt.Errorf("write dash.mpd: %w", werr)
	}
	return f.Close()
}

// generateSilenceAudio generates silent AAC DASH audio segments (ainit-stream0.m4s +
// achunk-stream0-NNNNN.m4s) in dir to cover the video duration already carved there.
func generateSilenceAudio(dir string, audioSegDurationTs int64) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	videoChunks := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "vchunk-stream0-") {
			videoChunks++
		}
	}
	if videoChunks == 0 {
		return nil
	}

	segSec := float64(audioSegDurationTs) / 48000.0
	totalSec := float64(videoChunks) * segSec

	mpdPath := filepath.Join(dir, "silence.mpd")
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi",
		"-i", "anullsrc=r=48000:cl=stereo",
		"-t", fmt.Sprintf("%.3f", totalSec),
		"-acodec", "aac",
		"-b:a", "128k",
		"-ar", "48000", "-ac", "2",
		"-f", "dash",
		"-seg_duration", fmt.Sprintf("%.3f", segSec),
		"-init_seg_name", "ainit-stream0.m4s",
		"-media_seg_name", "achunk-stream0-$Number%05d$.m4s",
		mpdPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg: %w\n%s", err, out)
	}
	return nil
}

// ---- Minimal local-file handlers ----

type localInputOpener struct{}

func (o *localInputOpener) Open(fd int64, url string) (goavpipe.InputHandler, error) {
	f, err := os.Open(url)
	if err != nil {
		return nil, err
	}
	return &localInput{f: f}, nil
}

type localInput struct {
	f *os.File
}

func (in *localInput) Read(buf []byte) (int, error) {
	n, err := in.f.Read(buf)
	if err != nil && err.Error() == "EOF" {
		return 0, nil
	}
	return n, err
}

func (in *localInput) Seek(offset int64, whence int) (int64, error) {
	return in.f.Seek(offset, whence)
}

func (in *localInput) Close() error {
	return in.f.Close()
}

func (in *localInput) Size() int64 {
	fi, err := in.f.Stat()
	if err != nil {
		return -1
	}
	return fi.Size()
}

func (in *localInput) Stat(streamIndex int, statType goavpipe.AVStatType, statArgs interface{}) error {
	return nil
}

type localMuxOutputOpener struct{}

func (o *localMuxOutputOpener) Open(filename string, fd int64, outType goavpipe.AVType) (goavpipe.OutputHandler, error) {
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	return &localOutput{f: f}, nil
}

type localOutput struct {
	f *os.File
}

func (out *localOutput) Write(buf []byte) (int, error) {
	return out.f.Write(buf)
}

func (out *localOutput) Seek(offset int64, whence int) (int64, error) {
	return out.f.Seek(offset, whence)
}

func (out *localOutput) Close() error {
	return out.f.Close()
}

func (out *localOutput) Stat(streamIndex int, avType goavpipe.AVType, statType goavpipe.AVStatType, statArgs interface{}) error {
	return nil
}

// ---- Mux spec builder ----

func buildAbrMuxSpec(audioOrdered []string, videoOrdered []string) string {
	// First line must be abr-mux.
	// Each following line: stream_type,stream_index,path
	var b strings.Builder
	b.WriteString("abr-mux\n")
	for _, p := range audioOrdered {
		b.WriteString("audio,1,")
		b.WriteString(p)
		b.WriteString("\n")
	}
	for _, p := range videoOrdered {
		b.WriteString("video,1,")
		b.WriteString(p)
		b.WriteString("\n")
	}
	return b.String()
}

// StitchWithAd inserts ad parts between program parts by ordering chunk paths.
// audioOrdered and videoOrdered should already be in final playback order:
// program-before -> ad -> program-after.
func StitchWithAd(outputMP4 string, audioOrdered []string, videoOrdered []string) error {
	muxSpec := buildAbrMuxSpec(audioOrdered, videoOrdered)

	// Register mux IO handlers keyed by output URL.
	goavpipe.InitUrlMuxIOHandler(outputMP4, &localInputOpener{}, &localMuxOutputOpener{})

	params := &goavpipe.XcParams{
		Url:             outputMP4,
		Format:          "mp4", // or "fmp4-segment"
		MuxingSpec:      muxSpec,
		DebugFrameLevel: false,
	}

	return avpipe.Mux(params)
}

// ---- DASH playlist writer ----

// DASHParams carries the stream-level metadata needed to write a valid MPD.
type DASHParams struct {
	// VideoTimescale is the video track timescale (e.g. 30000).
	VideoTimescale int64
	// VideoSegDurationTs is the nominal video segment duration in timescale units.
	VideoSegDurationTs int64
	// AudioTimescale is the audio sample rate used as timescale (e.g. 48000).
	AudioTimescale int64
	// AudioSegDurationTs is the nominal audio segment duration in sample-rate units.
	AudioSegDurationTs int64
	// Width / Height of the encoded video.
	Width, Height int
	// FrameRateNum / Den: e.g. 30000 / 1001 for 29.97, or 30 / 1 for 30 fps.
	FrameRateNum, FrameRateDen int
	// VideoBandwidth in bits/s.
	VideoBandwidth int
	// AudioBandwidth in bits/s.
	AudioBandwidth int
}

// DefaultDASHParams returns sensible defaults matching DefaultCarveParams output.
func DefaultDASHParams() DASHParams {
	return DASHParams{
		VideoTimescale:     30000,
		VideoSegDurationTs: 48000,
		AudioTimescale:     48000,
		AudioSegDurationTs: 96000,
		Width:              1280,
		Height:             720,
		FrameRateNum:       30,
		FrameRateDen:       1,
		VideoBandwidth:     2560000,
		AudioBandwidth:     128000,
	}
}

// WriteDASH materialises a DASH presentation in outDir from the already-ordered
// audio and video segment paths (first element is the init segment).
// All referenced files are hard-linked (or copied when cross-device) into outDir
// with sequentially-numbered names so the resulting directory is self-contained.
// A SegmentList-based MPD is written to outDir/playlist.mpd.
func WriteDASH(outDir string, audioOrdered, videoOrdered []string, dp DASHParams) error {
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return fmt.Errorf("create dash dir %s: %w", outDir, err)
	}

	// Stage video segments.
	vInit, vChunkNames, err := stageSegments(outDir, "v", videoOrdered)
	if err != nil {
		return fmt.Errorf("stage video: %w", err)
	}

	// Stage audio segments.
	aInit, aChunkNames, err := stageSegments(outDir, "a", audioOrdered)
	if err != nil {
		return fmt.Errorf("stage audio: %w", err)
	}

	// Compute total duration in seconds from chunk counts and nominal seg duration.
	totalSec := float64(len(vChunkNames)) * float64(dp.VideoSegDurationTs) / float64(dp.VideoTimescale)

	type segURL struct{ Media string }
	type adaptSet struct {
		ID         int
		MimeType   string
		Codecs     string
		Init       string
		Bandwidth  int
		Width      int
		Height     int
		FrameRate  string
		SampleRate int
		Channels   int
		SegDurSec  float64
		Timescale  int64
		SegDurTs   int64
		Segments   []segURL
	}

	frameRate := fmt.Sprintf("%d/%d", dp.FrameRateNum, dp.FrameRateDen)
	if dp.FrameRateDen == 1 {
		frameRate = fmt.Sprintf("%d", dp.FrameRateNum)
	}

	videoSet := adaptSet{
		ID:        1,
		MimeType:  "video/mp4",
		Codecs:    "avc1.640028",
		Init:      vInit,
		Bandwidth: dp.VideoBandwidth,
		Width:     dp.Width,
		Height:    dp.Height,
		FrameRate: frameRate,
		SegDurSec: float64(dp.VideoSegDurationTs) / float64(dp.VideoTimescale),
		Timescale: dp.VideoTimescale,
		SegDurTs:  dp.VideoSegDurationTs,
	}
	for _, n := range vChunkNames {
		videoSet.Segments = append(videoSet.Segments, segURL{n})
	}

	audioSet := adaptSet{
		ID:         2,
		MimeType:   "audio/mp4",
		Codecs:     "mp4a.40.2",
		Init:       aInit,
		Bandwidth:  dp.AudioBandwidth,
		SampleRate: int(dp.AudioTimescale),
		Channels:   2,
		SegDurSec:  float64(dp.AudioSegDurationTs) / float64(dp.AudioTimescale),
		Timescale:  dp.AudioTimescale,
		SegDurTs:   dp.AudioSegDurationTs,
	}
	for _, n := range aChunkNames {
		audioSet.Segments = append(audioSet.Segments, segURL{n})
	}

	mpdData := struct {
		TotalSec float64
		Video    adaptSet
		Audio    adaptSet
	}{totalSec, videoSet, audioSet}

	const mpdTmpl = `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"
     xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
     xsi:schemaLocation="urn:mpeg:dash:schema:mpd:2011 DASH-MPD.xsd"
     type="static"
     mediaPresentationDuration="PT{{printf "%.3f" .TotalSec}}S"
     minBufferTime="PT2S"
     profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">
  <Period>
    <AdaptationSet id="{{.Video.ID}}" mimeType="{{.Video.MimeType}}" codecs="{{.Video.Codecs}}"
                   width="{{.Video.Width}}" height="{{.Video.Height}}" frameRate="{{.Video.FrameRate}}"
                   startWithSAP="1" segmentAlignment="true">
      <Representation id="video0" bandwidth="{{.Video.Bandwidth}}">
        <SegmentList timescale="{{.Video.Timescale}}" duration="{{.Video.SegDurTs}}">
          <Initialization sourceURL="{{.Video.Init}}"/>
{{- range .Video.Segments}}
          <SegmentURL media="{{.Media}}"/>
{{- end}}
        </SegmentList>
      </Representation>
    </AdaptationSet>
    <AdaptationSet id="{{.Audio.ID}}" mimeType="{{.Audio.MimeType}}" codecs="{{.Audio.Codecs}}"
                   lang="und" segmentAlignment="true">
      <Representation id="audio0" bandwidth="{{.Audio.Bandwidth}}"
                      audioSamplingRate="{{.Audio.SampleRate}}" numChannels="{{.Audio.Channels}}">
        <SegmentList timescale="{{.Audio.Timescale}}" duration="{{.Audio.SegDurTs}}">
          <Initialization sourceURL="{{.Audio.Init}}"/>
{{- range .Audio.Segments}}
          <SegmentURL media="{{.Media}}"/>
{{- end}}
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>
`
	t, err := template.New("mpd").Parse(mpdTmpl)
	if err != nil {
		return fmt.Errorf("parse mpd template: %w", err)
	}

	mpdPath := filepath.Join(outDir, "playlist.mpd")
	f, err := os.OpenFile(mpdPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("create mpd: %w", err)
	}
	if err := t.Execute(f, mpdData); err != nil {
		_ = f.Close()
		return fmt.Errorf("write mpd: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close mpd: %w", err)
	}

	return nil
}

// stageSegments copies (or hard-links) all segments into outDir with new
// sequential names prefixed by trackPrefix ("v" or "a").
// Returns the init-segment filename and the slice of data-segment filenames.
func stageSegments(outDir, trackPrefix string, ordered []string) (initName string, chunkNames []string, err error) {
	if len(ordered) == 0 {
		return "", nil, fmt.Errorf("no segments for track %s", trackPrefix)
	}

	// First element is always the init segment.
	initName = trackPrefix + "init.m4s"
	if err := linkOrCopy(ordered[0], filepath.Join(outDir, initName)); err != nil {
		return "", nil, fmt.Errorf("stage init: %w", err)
	}

	chunkNames = make([]string, 0, len(ordered)-1)
	for i, src := range ordered[1:] {
		name := fmt.Sprintf("%schunk-%05d.m4s", trackPrefix, i+1)
		if err := linkOrCopy(src, filepath.Join(outDir, name)); err != nil {
			return "", nil, fmt.Errorf("stage chunk %d: %w", i+1, err)
		}
		chunkNames = append(chunkNames, name)
	}
	return initName, chunkNames, nil
}

// linkOrCopy hard-links src to dst (same device) or falls back to a byte copy.
func linkOrCopy(src, dst string) error {
	// Remove destination if it already exists (idempotent re-runs).
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	// Cross-device or unsupported — fall back to copy.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
