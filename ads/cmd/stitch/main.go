package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/eluv-io/avpipe/ads"
)

type mediaProfile struct {
	Width          int
	Height         int
	FrameRate      string
	AudioSampleHz  int
	HasVideoStream bool
}

func probeProfile(path string) (mediaProfile, error) {
	type ffProbeStream struct {
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		AvgFrameRate string `json:"avg_frame_rate"`
		RFrameRate   string `json:"r_frame_rate"`
		SampleRate   string `json:"sample_rate"`
	}
	type ffProbeOut struct {
		Streams []ffProbeStream `json:"streams"`
	}

	prof := mediaProfile{}

	videoCmd := exec.Command(
		"ffprobe", "-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height,avg_frame_rate,r_frame_rate",
		"-of", "json",
		path,
	)
	vout, err := videoCmd.Output()
	if err != nil {
		return prof, fmt.Errorf("ffprobe video stream (%s): %w", path, err)
	}

	var v ffProbeOut
	if err := json.Unmarshal(vout, &v); err != nil {
		return prof, fmt.Errorf("parse ffprobe video output (%s): %w", path, err)
	}
	if len(v.Streams) == 0 {
		return prof, fmt.Errorf("no video stream found in %s", path)
	}
	prof.Width = v.Streams[0].Width
	prof.Height = v.Streams[0].Height
	prof.FrameRate = v.Streams[0].AvgFrameRate
	if prof.FrameRate == "" || prof.FrameRate == "0/0" {
		prof.FrameRate = v.Streams[0].RFrameRate
	}
	prof.HasVideoStream = true

	audioCmd := exec.Command(
		"ffprobe", "-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "stream=sample_rate",
		"-of", "json",
		path,
	)
	aout, err := audioCmd.Output()
	if err == nil {
		var a ffProbeOut
		if jerr := json.Unmarshal(aout, &a); jerr == nil && len(a.Streams) > 0 {
			var sr int
			_, _ = fmt.Sscanf(a.Streams[0].SampleRate, "%d", &sr)
			prof.AudioSampleHz = sr
		}
	}

	return prof, nil
}

type adInsertSpec struct {
	Dir         string  `json:"dir"`
	InsertAtSec float64 `json:"insert_at_sec"`
}

type stitchSpec struct {
	OutputPath         string         `json:"output_path"`
	ProgramDir         string         `json:"program_dir"`
	Program2Dir        string         `json:"program2_dir,omitempty"`
	ProgramChunkSec    float64        `json:"program_chunk_sec"`
	Ads                []adInsertSpec `json:"ads"`
	VideoSegDurationTs int64          `json:"video_seg_ts,omitempty"`
	AudioSegDurationTs int64          `json:"audio_seg_ts,omitempty"`
}

func collectTrackFiles(dir, initName, chunkPrefix string) ([]string, error) {
	initPath := filepath.Join(dir, initName)
	if _, err := os.Stat(initPath); err != nil {
		return nil, fmt.Errorf("missing init segment %s: %w", initPath, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}

	chunks := make([]string, 0)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, chunkPrefix) && strings.HasSuffix(name, ".m4s") {
			chunks = append(chunks, filepath.Join(dir, name))
		}
	}

	if len(chunks) == 0 {
		return nil, fmt.Errorf("no chunks found in %s with prefix %s", dir, chunkPrefix)
	}

	sort.Strings(chunks)
	parts := make([]string, 0, len(chunks)+1)
	parts = append(parts, initPath)
	parts = append(parts, chunks...)
	return parts, nil
}

func collectAV(dir string) ([]string, []string, error) {
	audio, err := collectTrackFiles(dir, "ainit-stream0.m4s", "achunk-stream0-")
	if err != nil {
		return nil, nil, fmt.Errorf("ad scan failed: %w", err)
	}

	video, err := collectTrackFiles(dir, "vinit-stream0.m4s", "vchunk-stream0-")
	if err != nil {
		return nil, nil, err
	}

	return audio, video, nil
}

func chunksOnly(segs []string) []string {
	if len(segs) > 1 {
		return segs[1:]
	}
	return nil
}

func splitByChunkIndex(segs []string, split int) ([]string, []string) {
	chunks := chunksOnly(segs)
	if split < 0 {
		split = 0
	}
	if split > len(chunks) {
		split = len(chunks)
	}
	return chunks[:split], chunks[split:]
}

func probeDurationFromSegments(segs []string) (float64, error) {
	if len(segs) == 0 {
		return 0, fmt.Errorf("no segments to probe")
	}

	tmp, err := os.CreateTemp("", "stitch-probe-*.m4s")
	if err != nil {
		return 0, fmt.Errorf("create probe temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	for _, p := range segs {
		f, err := os.Open(p)
		if err != nil {
			tmp.Close()
			return 0, fmt.Errorf("open segment %s: %w", p, err)
		}
		if _, err := io.Copy(tmp, f); err != nil {
			f.Close()
			tmp.Close()
			return 0, fmt.Errorf("append segment %s: %w", p, err)
		}
		f.Close()
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("close probe temp file: %w", err)
	}

	cmd := exec.Command(
		"ffprobe", "-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1",
		tmpPath,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe duration %s: %w", tmpPath, err)
	}

	durStr := strings.TrimSpace(string(bytes.TrimSpace(out)))
	dur, err := strconv.ParseFloat(durStr, 64)
	if err != nil {
		return 0, fmt.Errorf("parse ffprobe duration %q: %w", durStr, err)
	}
	return dur, nil
}

func mergeWithInsertions(
	progAudio []string,
	progVideo []string,
	insertions []struct {
		audioChunkIndex int
		videoChunkIndex int
		audio           []string
		video           []string
	},
	prog2Audio []string,
	prog2Video []string,
) ([]string, []string) {
	sort.Slice(insertions, func(i, j int) bool {
		if insertions[i].videoChunkIndex == insertions[j].videoChunkIndex {
			return insertions[i].audioChunkIndex < insertions[j].audioChunkIndex
		}
		return insertions[i].videoChunkIndex < insertions[j].videoChunkIndex
	})

	audio := make([]string, 0, len(progAudio)+len(prog2Audio)+64)
	video := make([]string, 0, len(progVideo)+len(prog2Video)+64)
	audio = append(audio, progAudio[:1]...)
	video = append(video, progVideo[:1]...)

	progAudioChunks := chunksOnly(progAudio)
	progVideoChunks := chunksOnly(progVideo)
	prevAudio := 0
	prevVideo := 0
	for _, ins := range insertions {
		audioSplit := ins.audioChunkIndex
		if audioSplit < prevAudio {
			audioSplit = prevAudio
		}
		if audioSplit > len(progAudioChunks) {
			audioSplit = len(progAudioChunks)
		}

		videoSplit := ins.videoChunkIndex
		if videoSplit < prevVideo {
			videoSplit = prevVideo
		}
		if videoSplit > len(progVideoChunks) {
			videoSplit = len(progVideoChunks)
		}

		audio = append(audio, progAudioChunks[prevAudio:audioSplit]...)
		video = append(video, progVideoChunks[prevVideo:videoSplit]...)
		audio = append(audio, ins.audio...)
		video = append(video, ins.video...)
		prevAudio = audioSplit
		prevVideo = videoSplit
	}

	audio = append(audio, progAudioChunks[prevAudio:]...)
	video = append(video, progVideoChunks[prevVideo:]...)
	audio = append(audio, chunksOnly(prog2Audio)...)
	video = append(video, chunksOnly(prog2Video)...)

	return audio, video
}

func buildFromSpec(specPath string, defaultOut string) (string, []string, []string, error) {
	b, err := os.ReadFile(specPath)
	if err != nil {
		return "", nil, nil, fmt.Errorf("read spec %s: %w", specPath, err)
	}

	var s stitchSpec
	if err := json.Unmarshal(b, &s); err != nil {
		return "", nil, nil, fmt.Errorf("parse spec %s: %w", specPath, err)
	}
	if s.ProgramDir == "" {
		return "", nil, nil, fmt.Errorf("spec.program_dir is required")
	}
	if s.ProgramChunkSec <= 0 {
		return "", nil, nil, fmt.Errorf("spec.program_chunk_sec must be > 0")
	}

	progAudio, progVideo, err := collectAV(s.ProgramDir)
	if err != nil {
		return "", nil, nil, fmt.Errorf("program scan failed: %w", err)
	}

	insertions := make([]struct {
		audioChunkIndex int
		videoChunkIndex int
		audio           []string
		video           []string
	}, 0, len(s.Ads))

	progAudioChunks := chunksOnly(progAudio)
	progVideoChunks := chunksOnly(progVideo)
	videoChunkSec := s.ProgramChunkSec
	audioChunkSec := s.ProgramChunkSec
	if len(progAudioChunks) > 0 && len(progVideoChunks) > 0 {
		audioChunkSec = s.ProgramChunkSec * float64(len(progVideoChunks)) / float64(len(progAudioChunks))
	}

	insertChunkIndex := func(insertAtSec, chunkSec float64, max int) int {
		if chunkSec <= 0 {
			return 0
		}
		idx := int(math.Ceil(insertAtSec / chunkSec))
		if idx < 0 {
			idx = 0
		}
		if idx > max {
			idx = max
		}
		return idx
	}

	for _, ad := range s.Ads {
		if ad.Dir == "" {
			return "", nil, nil, fmt.Errorf("each ad entry needs dir")
		}
		if ad.InsertAtSec < 0 {
			return "", nil, nil, fmt.Errorf("insert_at_sec must be >= 0 for ad %s", ad.Dir)
		}
		aud, vid, err := collectAV(ad.Dir)
		if err != nil {
			return "", nil, nil, fmt.Errorf("ad scan failed for %s: %w", ad.Dir, err)
		}

		adAudioChunks := chunksOnly(aud)
		adVideoChunks := chunksOnly(vid)

		videoInsertIdx := insertChunkIndex(ad.InsertAtSec, videoChunkSec, len(progVideoChunks))
		audioInsertIdx := insertChunkIndex(ad.InsertAtSec, audioChunkSec, len(progAudioChunks))

		// Keep all ad chunks from both tracks. Estimating ad duration from program
		// chunk cadence is unreliable because ad segment cadence can differ.
		adVideoKeep := len(adVideoChunks)
		adAudioKeep := len(adAudioChunks)

		adAudioInsert := append([]string(nil), adAudioChunks[:adAudioKeep]...)
		if len(aud) > 1 && len(vid) > 1 && audioChunkSec > 0 {
			adVideoDurSec, err := probeDurationFromSegments(vid)
			if err == nil && adVideoDurSec > 0 {
				requiredAudioChunks := int(math.Ceil(adVideoDurSec / audioChunkSec))
				if requiredAudioChunks < 1 {
					requiredAudioChunks = 1
				}
				if requiredAudioChunks < len(adAudioInsert) {
					adAudioInsert = adAudioInsert[:requiredAudioChunks]
				} else if requiredAudioChunks > len(adAudioInsert) && len(adAudioInsert) > 0 {
					last := adAudioInsert[len(adAudioInsert)-1]
					for len(adAudioInsert) < requiredAudioChunks {
						adAudioInsert = append(adAudioInsert, last)
					}
				}
			}
		}

		insertions = append(insertions, struct {
			audioChunkIndex int
			videoChunkIndex int
			audio           []string
			video           []string
		}{
			audioChunkIndex: audioInsertIdx,
			videoChunkIndex: videoInsertIdx,
			audio:           adAudioInsert,
			video:           adVideoChunks[:adVideoKeep],
		})
	}

	var prog2Audio, prog2Video []string
	if s.Program2Dir != "" {
		if _, err := os.Stat(s.Program2Dir); err == nil {
			prog2Audio, prog2Video, err = collectAV(s.Program2Dir)
			if err != nil {
				return "", nil, nil, fmt.Errorf("program2 scan failed: %w", err)
			}
		}
	}

	audio, video := mergeWithInsertions(progAudio, progVideo, insertions, prog2Audio, prog2Video)
	out := s.OutputPath
	if out == "" {
		out = defaultOut
	}
	return out, audio, video, nil
}

func main() {
	// -carve mode: transcode raw MP4s into m4s chunks first.
	carve := flag.Bool("carve", false, "transcode -prog and -ad MP4s into m4s chunks before stitching")
	progMP4 := flag.String("prog", "", "program MP4 input (only used with -carve)")
	adMP4 := flag.String("ad", "", "ad MP4 input (only used with -carve)")
	prog2MP4 := flag.String("prog2", "", "optional second program MP4 input (only used with -carve)")
	vidSegTs := flag.Int64("video-seg-ts", 48000, "video segment duration in timebase units")
	audSegTs := flag.Int64("audio-seg-ts", 96000, "audio segment duration in timebase units")
	forceKeyInt := flag.Int("keyint", 48, "force IDR keyframe interval (frames)")
	encHeight := flag.Int("enc-height", -1, "encode height (-1 = keep source)")
	encWidth := flag.Int("enc-width", -1, "encode width (-1 = keep source)")
	forceFPS := flag.Int("force-fps", 0, "force output frame rate (e.g. 30); 0 = keep source")

	// -stitch mode: assemble pre-carved m4s chunk directories.
	progDir := flag.String("prog-dir", "out/prog", "program segment directory")
	adDir := flag.String("ad-dir", "out/ad", "ad segment directory")
	prog2Dir := flag.String("prog2-dir", "out/prog2", "optional second program segment directory")
	out := flag.String("out", "out/final_with_ad.mp4", "output MP4 path")
	spec := flag.String("spec", "", "optional JSON stitch spec file; when set it drives insertion timeline")

	// -dash mode: write a DASH playlist directory instead of (or in addition to) an MP4.
	dashDir := flag.String("dash-dir", "", "when set, write DASH segments + MPD to this directory instead of muxing to MP4")
	serveAddr := flag.String("serve", "", "when set (e.g. :8080) start an HTTP server serving -dash-dir; implies -dash-dir defaults to 'out/dash'")
	flag.Parse()

	if *carve {
		if *progMP4 == "" || *adMP4 == "" {
			fmt.Fprintln(os.Stderr, "-prog and -ad are required with -carve")
			os.Exit(1)
		}

		// Normalize all carved inputs to the same profile so mux+re-carve does not
		// cross stream-property boundaries (e.g. 2160p -> 1080p or 48k -> 44.1k).
		progProfile, err := probeProfile(*progMP4)
		if err != nil {
			panic(fmt.Errorf("probe program input %s: %w", *progMP4, err))
		}
		targetW := *encWidth
		targetH := *encHeight
		if targetW <= 0 {
			targetW = progProfile.Width
		}
		if targetH <= 0 {
			targetH = progProfile.Height
		}
		targetFPS := ""
		if *forceFPS > 0 {
			targetFPS = fmt.Sprintf("%d", *forceFPS)
		} else if progProfile.FrameRate != "" && progProfile.FrameRate != "0/0" {
			targetFPS = progProfile.FrameRate
		}

		cp := ads.DefaultCarveParams("")
		cp.VideoSegDurationTs = *vidSegTs
		cp.AudioSegDurationTs = *audSegTs
		cp.ForceKeyInt = int32(*forceKeyInt)
		cp.EncHeight = int32(targetH)
		cp.EncWidth = int32(targetW)
		if targetFPS != "" {
			cp.VideoFilter = fmt.Sprintf("fps=%s", targetFPS)
		}
		fmt.Printf("normalizing carve profile: %dx%d, fps=%s, audio=48000Hz stereo\n", targetW, targetH, targetFPS)

		for _, pair := range []struct{ mp4, dir string }{
			{*progMP4, *progDir},
			{*adMP4, *adDir},
			{*prog2MP4, *prog2Dir},
		} {
			if pair.mp4 == "" {
				continue
			}
			if err := os.RemoveAll(pair.dir); err != nil {
				panic(fmt.Errorf("failed to clean output dir %s: %w", pair.dir, err))
			}
			cp.OutDir = pair.dir
			fmt.Printf("carving %s -> %s\n", pair.mp4, pair.dir)
			if err := ads.Carve(pair.mp4, cp); err != nil {
				panic(fmt.Errorf("carve failed: %w", err))
			}
		}
	}

	stitchOut := *out
	var audio []string
	var video []string
	var err error

	if *spec != "" {
		stitchOut, audio, video, err = buildFromSpec(*spec, *out)
		if err != nil {
			panic(err)
		}
	} else {
		progAudio, progVideo, err := collectAV(*progDir)
		if err != nil {
			panic(fmt.Errorf("program scan failed: %w", err))
		}

		adAudio, adVideo, err := collectAV(*adDir)
		if err != nil {
			panic(fmt.Errorf("ad scan failed: %w", err))
		}

		audio = make([]string, 0, len(progAudio)+len(adAudio)+32)
		video = make([]string, 0, len(progVideo)+len(adVideo)+32)
		audio = append(audio, progAudio...)           // prog supplies the init + chunks
		audio = append(audio, chunksOnly(adAudio)...) // ad: chunks only (no second ainit)
		video = append(video, progVideo...)           // prog supplies the init + chunks
		video = append(video, chunksOnly(adVideo)...) // ad: chunks only (no second vinit)

		if *prog2Dir != "" {
			if _, err := os.Stat(*prog2Dir); err == nil {
				prog2Audio, prog2Video, err := collectAV(*prog2Dir)
				if err != nil {
					panic(fmt.Errorf("program2 scan failed: %w", err))
				}
				audio = append(audio, chunksOnly(prog2Audio)...)
				video = append(video, chunksOnly(prog2Video)...)
			}
		}
	}

	// If -serve is set, default -dash-dir so the user doesn't have to specify both.
	if *serveAddr != "" && *dashDir == "" {
		*dashDir = "out/dash"
	}

	if *dashDir != "" {
		// Robust DASH mode: first stitch to a single MP4 timeline, then carve
		// that MP4 back into DASH. This avoids timestamp discontinuities between
		// independently-carved program/ad chunks.
		tmpDir, err := os.MkdirTemp("", "stitch-dash-")
		if err != nil {
			panic(fmt.Errorf("create temp dir: %w", err))
		}
		defer os.RemoveAll(tmpDir)

		tmpMP4 := filepath.Join(tmpDir, "stitched.mp4")
		if err := ads.StitchWithAd(tmpMP4, audio, video); err != nil {
			panic(fmt.Errorf("intermediate mux failed: %w", err))
		}

		if err := os.RemoveAll(*dashDir); err != nil {
			panic(fmt.Errorf("clean dash dir %s: %w", *dashDir, err))
		}

		cp := ads.DefaultCarveParams(*dashDir)
		cp.VideoSegDurationTs = *vidSegTs
		cp.AudioSegDurationTs = *audSegTs
		cp.ForceKeyInt = int32(*forceKeyInt)
		cp.EncHeight = int32(*encHeight)
		cp.EncWidth = int32(*encWidth)
		if *forceFPS > 0 {
			cp.VideoFilter = fmt.Sprintf("fps=%d", *forceFPS)
		}

		if err := ads.Carve(tmpMP4, cp); err != nil {
			panic(fmt.Errorf("dash carve failed: %w", err))
		}

		// Avpipe runs separate video and audio passes; each writes its own
		// dash.mpd, so the audio pass overwrites the video one, leaving an
		// audio-only manifest. Rewrite it now with both streams.
		if err := ads.OverwriteDASHManifest(*dashDir, cp); err != nil {
			panic(fmt.Errorf("rewrite DASH manifest: %w", err))
		}

		mpdPath := filepath.Join(*dashDir, "dash.mpd")
		fmt.Printf("DASH playlist written to %s\n", mpdPath)
		if *serveAddr != "" {
			fmt.Printf("serving %s at http://%s/\n", *dashDir, *serveAddr)
			fmt.Printf("MPD URL: http://%s/dash.mpd\n", *serveAddr)
			fs := http.FileServer(http.Dir(*dashDir))
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Allow any origin so browser DASH players can fetch segments.
				w.Header().Set("Access-Control-Allow-Origin", "*")
				fs.ServeHTTP(w, r)
			})
			if err := http.ListenAndServe(*serveAddr, handler); err != nil {
				panic(fmt.Errorf("http server: %w", err))
			}
		}
		return
	}

	if err := ads.StitchWithAd(stitchOut, audio, video); err != nil {
		panic(fmt.Errorf("mux failed: %w", err))
	}
	fmt.Printf("created %s\n", stitchOut)
}
