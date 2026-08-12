package channel

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/server"
)

type noopManager struct{}

func (noopManager) CreateChannel(*entity.ChannelConfig, bool) error { return nil }
func (noopManager) StopChannel(string) error                        { return nil }
func (noopManager) PauseChannel(string) error                       { return nil }
func (noopManager) ResumeChannel(string) error                      { return nil }
func (noopManager) ChannelInfo() []*entity.ChannelInfo              { return nil }
func (noopManager) Publish(string, *entity.ChannelInfo)             {}
func (noopManager) Subscriber(http.ResponseWriter, *http.Request)   {}
func (noopManager) LoadConfig() error                               { return nil }
func (noopManager) SaveConfig() error                               { return nil }
func (noopManager) Shutdown(context.Context) error                  { return nil }

func init() {
	server.Manager = noopManager{}
}

func TestHandleInitSegmentRenamesFileToMP4(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ch := New(&entity.ChannelConfig{
		Username: "alice",
		Pattern:  filepath.Join(dir, "recording"),
	})
	ch.StreamedAt = 1

	if err := ch.CreateNewFile(filepath.Join(dir, "recording")); err != nil {
		t.Fatalf("CreateNewFile() error = %v", err)
	}

	initSegment := []byte("ftyp-test-moov")
	if err := ch.HandleInitSegment(initSegment); err != nil {
		t.Fatalf("HandleInitSegment() error = %v", err)
	}
	t.Cleanup(func() { _ = ch.Cleanup() })

	if _, err := os.Stat(filepath.Join(dir, "recording.ts")); !os.IsNotExist(err) {
		t.Fatalf("expected .ts file to be renamed, stat err = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "recording.mp4"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != string(initSegment) {
		t.Fatalf("mp4 contents = %q, want %q", string(got), string(initSegment))
	}
}

func TestCreateNewFileWritesInitSegmentForRotatedFMP4Files(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	initSegment := []byte("ftyp-test-moov")
	ch := New(&entity.ChannelConfig{
		Username: "alice",
		Pattern:  filepath.Join(dir, "recording"),
	})
	ch.InitSegment = initSegment

	if err := ch.CreateNewFile(filepath.Join(dir, "rotated")); err != nil {
		t.Fatalf("CreateNewFile() error = %v", err)
	}
	t.Cleanup(func() { _ = ch.Cleanup() })

	got, err := os.ReadFile(filepath.Join(dir, "rotated.mp4"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != string(initSegment) {
		t.Fatalf("mp4 contents = %q, want %q", string(got), string(initSegment))
	}
}

func TestCreateNewFileDoesNotAppendToExistingRecording(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "recording")
	oldPath := base + ".ts"
	if err := os.WriteFile(oldPath, []byte("old recording"), 0644); err != nil {
		t.Fatal(err)
	}
	ch := New(&entity.ChannelConfig{Username: "alice", Pattern: base})
	if err := ch.CreateNewFile(base); err != nil {
		t.Fatalf("CreateNewFile() error = %v", err)
	}
	t.Cleanup(ch.Stop)
	if ch.File.Name() != base+" (1).ts" {
		t.Fatalf("new path = %q, want collision suffix", ch.File.Name())
	}
	got, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old recording" {
		t.Fatalf("existing recording changed: %q", got)
	}
}

func TestCreateNewRealtimeFileDoesNotTruncateInterruptedRecording(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "recording")
	oldPath := realtimeWorkingPath(base)
	if err := os.WriteFile(oldPath, []byte("recoverable data"), 0644); err != nil {
		t.Fatal(err)
	}
	ch := New(&entity.ChannelConfig{Username: "alice", Pattern: base})
	ch.RealtimeMux = true
	ch.CombinedInit = []byte("new init")
	if err := ch.CreateNewFile(base); err != nil {
		t.Fatalf("CreateNewFile() error = %v", err)
	}
	t.Cleanup(ch.Stop)
	if ch.File.Name() != realtimeWorkingPath(base+" (1)") {
		t.Fatalf("new path = %q, want collision suffix", ch.File.Name())
	}
	got, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "recoverable data" {
		t.Fatalf("interrupted recording changed: %q", got)
	}
}

// buildFragmentedMP4 creates a minimal valid fragmented MP4 in memory with one track and one sample.
func buildFragmentedMP4(t *testing.T, mediaType string, timescale uint32, sampleData []byte) []byte {
	t.Helper()
	return buildFragmentedMP4WithSamples(t, mediaType, timescale, []byte(sampleData), 1)
}

// buildFragmentedMP4WithSamples creates a fragmented MP4 with the requested
// number of single-sample fragments, each holding sampleData and lasting one
// second of media (Dur = timescale per sample).
func buildFragmentedMP4WithSamples(t *testing.T, mediaType string, timescale uint32, sampleData []byte, fragments int) []byte {
	t.Helper()

	init := mp4.CreateEmptyInit()
	init.AddEmptyTrack(timescale, mediaType, "und")
	if err := init.TweakSingleTrakLive(); err != nil {
		t.Fatalf("TweakSingleTrakLive(%s) error = %v", mediaType, err)
	}

	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		t.Fatalf("encode init(%s) error = %v", mediaType, err)
	}

	for i := 0; i < fragments; i++ {
		seg := mp4.NewMediaSegmentWithoutStyp()
		frag, err := mp4.CreateFragment(uint32(i+1), 1)
		if err != nil {
			t.Fatalf("CreateFragment(%s) error = %v", mediaType, err)
		}
		if err := frag.AddFullSampleToTrack(mp4.FullSample{
			Sample:     mp4.Sample{Flags: mp4.SyncSampleFlags, Dur: timescale, Size: uint32(len(sampleData))},
			DecodeTime: uint64(i) * uint64(timescale),
			Data:       sampleData,
		}, 1); err != nil {
			t.Fatalf("AddFullSampleToTrack(%s) error = %v", mediaType, err)
		}
		seg.AddFragment(frag)
		if err := seg.Encode(&buf); err != nil {
			t.Fatalf("encode segment(%s) error = %v", mediaType, err)
		}
	}
	return buf.Bytes()
}

func markSeparateTrackMediaWritten(ch *Channel, videoData, audioData []byte) {
	ch.videoMediaBytes = len(videoData)
	ch.audioMediaBytes = len(audioData)
}

func splitInitAndMedia(t *testing.T, data []byte) ([]byte, []byte) {
	t.Helper()
	file, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeFile() error = %v", err)
	}
	var initBuf, mediaBuf bytes.Buffer
	if err := file.Init.Encode(&initBuf); err != nil {
		t.Fatalf("encode init: %v", err)
	}
	if len(file.Segments) != 1 {
		t.Fatalf("segments = %d, want 1", len(file.Segments))
	}
	if err := file.Segments[0].Encode(&mediaBuf); err != nil {
		t.Fatalf("encode media: %v", err)
	}
	return initBuf.Bytes(), mediaBuf.Bytes()
}

func TestRealtimeMuxProducesPlayableTwoTrackFragmentedMP4(t *testing.T) {
	videoInit, videoMedia := splitInitAndMedia(t, buildFragmentedMP4(t, "video", 90000, []byte{0, 0, 0, 1, 0x67}))
	audioInit, audioMedia := splitInitAndMedia(t, buildFragmentedMP4(t, "audio", 48000, []byte{0xff, 0xf1}))
	combinedInit, err := combineInitSegments(videoInit, audioInit)
	if err != nil {
		t.Fatalf("combineInitSegments() error = %v", err)
	}
	combinedMedia, err := combineMediaSegments(videoInit, audioInit, videoMedia, audioMedia, 1)
	if err != nil {
		t.Fatalf("combineMediaSegments() error = %v", err)
	}
	output := append(append([]byte(nil), combinedInit...), combinedMedia...)
	file, err := mp4.DecodeFile(bytes.NewReader(output))
	if err != nil {
		t.Fatalf("combined MP4 is not decodable: %v", err)
	}
	if file.Init == nil || len(file.Init.Moov.Traks) != 2 {
		t.Fatalf("combined MP4 tracks = %d, want 2", len(file.Init.Moov.Traks))
	}
	if len(file.Segments) != 1 || len(file.Segments[0].Fragments) != 1 {
		t.Fatalf("combined MP4 does not contain one realtime fragment")
	}
}

func TestRealtimeMuxRebasesAbsoluteTimelineWithoutLosingAVOffset(t *testing.T) {
	videoInit, videoMedia := splitInitAndMedia(t, buildFragmentedMP4(t, "video", 90000, []byte{0, 0, 0, 1, 0x67}))
	audioInit, audioMedia := splitInitAndMedia(t, buildFragmentedMP4(t, "audio", 48000, []byte{0xff, 0xf1}))
	videoMedia = setFragmentDecodeTime(t, videoMedia, 900*90000)
	audioMedia = setFragmentDecodeTime(t, audioMedia, 900*48000+12000) // audio begins 250ms later

	muxer, err := newRealtimeMuxer(videoInit, audioInit)
	if err != nil {
		t.Fatal(err)
	}
	combined, err := muxer.combineMedia(videoMedia, audioMedia, 1)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := mp4.DecodeFile(bytes.NewReader(combined))
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Segments) != 1 || len(decoded.Segments[0].Fragments) != 1 {
		t.Fatalf("unexpected combined media structure")
	}

	starts := map[uint32]uint64{}
	for _, traf := range decoded.Segments[0].Fragments[0].Moof.Trafs {
		starts[traf.Tfhd.TrackID] = traf.Tfdt.BaseMediaDecodeTime()
	}
	if got := starts[videoTrackID]; got != 0 {
		t.Fatalf("video TFDT = %d, want 0", got)
	}
	if got := starts[audioTrackID]; got != 12000 {
		t.Fatalf("audio TFDT = %d, want 12000 (250ms A/V offset)", got)
	}
}

func setFragmentDecodeTime(t *testing.T, media []byte, decodeTime uint64) []byte {
	t.Helper()
	decoded, err := mp4.DecodeFile(bytes.NewReader(media))
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Segments) != 1 || len(decoded.Segments[0].Fragments) != 1 {
		t.Fatalf("unexpected media structure")
	}
	for _, traf := range decoded.Segments[0].Fragments[0].Moof.Trafs {
		traf.Tfdt.SetBaseMediaDecodeTime(decodeTime)
	}
	var output bytes.Buffer
	if err := decoded.Segments[0].Encode(&output); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestBuildRealtimeFinalizeArgsCreatesZeroBasedFaststartCopy(t *testing.T) {
	t.Parallel()
	args := buildRealtimeFinalizeArgs("input.recording.mp4", "output.mp4")
	for _, want := range []string{"-c", "copy", "-copyts", "-start_at_zero", "-avoid_negative_ts", "make_zero", "-movflags", "+faststart", "-n"} {
		if !containsArg(args, want) {
			t.Fatalf("finalize args = %v, missing %q", args, want)
		}
	}
	if args[len(args)-1] != "output.mp4" {
		t.Fatalf("finalize output = %q, want output.mp4", args[len(args)-1])
	}
}

func TestOptimizeRealtimeMP4FailurePreservesSource(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	source := filepath.Join(dir, "recording.recording.mp4")
	desired := filepath.Join(dir, "recording.mp4")
	want := []byte("only recoverable recording")
	if err := os.WriteFile(source, want, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := optimizeRealtimeMP4(source, desired); err == nil {
		t.Fatal("optimizeRealtimeMP4() unexpectedly succeeded without ffmpeg")
	}
	got, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("source was not preserved: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("source changed: got %q want %q", got, want)
	}
	if _, err := os.Stat(desired); !os.IsNotExist(err) {
		t.Fatalf("unexpected final output after failure: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".finalize-*.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary finalize files left behind: %v", matches)
	}
}

func TestFinalizeRealtimeRecordingFallsBackWithoutLosingData(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	source := filepath.Join(dir, "recording.recording.mp4")
	desired := filepath.Join(dir, "recording.mp4")
	want := []byte("recoverable fragmented recording")
	if err := os.WriteFile(source, want, 0644); err != nil {
		t.Fatal(err)
	}

	ch := New(&entity.ChannelConfig{Username: "alice", Pattern: filepath.Join(dir, "recording")})
	ch.finalizeRealtimeRecording(source, desired)
	ch.compressWG.Wait()
	t.Cleanup(ch.Stop)

	got, err := os.ReadFile(desired)
	if err != nil {
		t.Fatalf("fallback recording missing: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("fallback recording changed: got %q want %q", got, want)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("working path still exists after fallback commit: %v", err)
	}
}

func TestOptimizeRealtimeMP4ProducesIndexedFaststartFile(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed")
	}

	dir := t.TempDir()
	source := filepath.Join(dir, "recording.recording.mp4")
	desired := filepath.Join(dir, "recording.mp4")
	generate := exec.Command(ffmpeg,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000",
		"-t", "0.5", "-c:v", "mpeg4", "-c:a", "aac",
		"-output_ts_offset", "120", "-movflags", "+frag_keyframe+empty_moov", source,
	)
	if output, err := generate.CombinedOutput(); err != nil {
		t.Skipf("cannot generate ffmpeg fixture: %v: %s", err, output)
	}

	finalPath, err := optimizeRealtimeMP4(source, desired)
	if err != nil {
		t.Fatalf("optimizeRealtimeMP4() error = %v", err)
	}
	if finalPath != desired {
		t.Fatalf("final path = %q, want %q", finalPath, desired)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source must remain until caller commits cleanup: %v", err)
	}
	if err := validateOptimizedMP4(finalPath, 0); err != nil {
		t.Fatalf("validateOptimizedMP4() error = %v", err)
	}

	probe := exec.Command(ffprobe, "-v", "error", "-show_entries", "format=start_time", "-of", "csv=p=0", finalPath)
	output, err := probe.Output()
	if err != nil {
		t.Fatalf("ffprobe output: %v", err)
	}
	start, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil {
		t.Fatalf("parse start time %q: %v", output, err)
	}
	if start < -0.05 || start > 0.05 {
		t.Fatalf("optimized start time = %.3fs, want approximately zero", start)
	}
}

func TestRecoverInterruptedRealtimeMP4(t *testing.T) {
	dir := t.TempDir()
	videoInit, videoMedia := splitInitAndMedia(t, buildFragmentedMP4(t, "video", 90000, []byte{0, 0, 0, 1, 0x67}))
	audioInit, audioMedia := splitInitAndMedia(t, buildFragmentedMP4(t, "audio", 48000, []byte{0xff, 0xf1}))
	initData, err := combineInitSegments(videoInit, audioInit)
	if err != nil {
		t.Fatal(err)
	}
	mediaData, err := combineMediaSegments(videoInit, audioInit, videoMedia, audioMedia, 1)
	if err != nil {
		t.Fatal(err)
	}
	working := filepath.Join(dir, "recording.recording.mp4")
	if err := os.WriteFile(working, append(initData, mediaData...), 0644); err != nil {
		t.Fatal(err)
	}
	if err := RecoverInterruptedRecordings(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "recording.mp4")); err != nil {
		t.Fatalf("recovered file missing: %v", err)
	}
}

func TestRecoverQuarantinesCorruptRealtimeMP4(t *testing.T) {
	dir := t.TempDir()
	working := filepath.Join(dir, "broken.recording.mp4")
	if err := os.WriteFile(working, []byte("not an mp4"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := RecoverInterruptedRecordings(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(working + ".corrupt"); err != nil {
		t.Fatalf("quarantined file missing: %v", err)
	}
}

func TestRecoveryDoesNotDescendIntoSessionArchives(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "historical.mp4.session")
	if err := os.MkdirAll(filepath.Join(archive, "parts"), 0755); err != nil {
		t.Fatal(err)
	}
	archivedRecording := filepath.Join(archive, "archived.recording.mp4")
	archivedTemporary := filepath.Join(archive, "parts", ".finalize-archived.mp4")
	for path, data := range map[string][]byte{
		archivedRecording: []byte("archive recording sentinel"),
		archivedTemporary: []byte("archive part sentinel"),
	} {
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	rootRecording := filepath.Join(dir, "broken.recording.mp4")
	if err := os.WriteFile(rootRecording, []byte("not an mp4"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RecoverInterruptedRecordings(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rootRecording + ".corrupt"); err != nil {
		t.Fatalf("root recording was not processed: %v", err)
	}
	for _, path := range []string{archivedRecording, archivedTemporary} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("session archive entry was changed: %s: %v", path, err)
		}
	}
}

func TestRealtimeSyncBatchesFragments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recording.mp4")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	ch := &Channel{File: file, LastFileSync: time.Now(), FragmentsSinceSync: realtimeSyncFragments - 1}
	if err := ch.syncRealtimeFile(false); err != nil {
		t.Fatal(err)
	}
	if ch.FragmentsSinceSync != realtimeSyncFragments-1 {
		t.Fatal("file synced before fragment threshold")
	}
	ch.FragmentsSinceSync++
	if err := ch.syncRealtimeFile(false); err != nil {
		t.Fatal(err)
	}
	if ch.FragmentsSinceSync != 0 {
		t.Fatal("file was not synced at fragment threshold")
	}
	_ = file.Close()
}

func TestResumeIsIdempotentAndStopTerminatesPublisher(t *testing.T) {
	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"room_status":"offline"}`))
	}))
	t.Cleanup(api.Close)
	oldConfig := server.Config
	server.Config = &entity.Config{Domain: api.URL + "/", Interval: 1}
	t.Cleanup(func() { server.Config = oldConfig })

	ch := New(&entity.ChannelConfig{Username: "alice", Pattern: filepath.Join(t.TempDir(), "recording")})
	ch.Resume(0)
	ch.Resume(0)
	time.Sleep(100 * time.Millisecond)
	if got := requests.Load(); got != 1 {
		t.Fatalf("API requests = %d, want 1 monitor", got)
	}
	ch.Stop()
	select {
	case <-ch.done:
	default:
		t.Fatal("publisher was not terminated")
	}
}

func TestMoveFileUniqueNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	desired := filepath.Join(dir, "recording.mp4")
	if err := os.WriteFile(desired, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "source.mp4")
	if err := os.WriteFile(source, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	actual, err := moveFileUnique(source, desired)
	if err != nil {
		t.Fatal(err)
	}
	if actual == desired {
		t.Fatal("existing destination was overwritten")
	}
	oldData, _ := os.ReadFile(desired)
	newData, _ := os.ReadFile(actual)
	if string(oldData) != "old" || string(newData) != "new" {
		t.Fatalf("contents old/new = %q/%q", oldData, newData)
	}
}

func TestCleanupNativeMuxesSeparateTracksWhenFFmpegUnavailable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	videoMP4 := buildFragmentedMP4(t, "video", 90000, []byte{0x00, 0x00, 0x00, 0x01, 0x67}) // fake NAL unit
	audioMP4 := buildFragmentedMP4(t, "audio", 44100, []byte{0xFF, 0xF1})                   // fake AAC frame

	base := filepath.Join(dir, "recording")
	ch := New(&entity.ChannelConfig{
		Username: "alice",
		Pattern:  base,
	})
	ch.HasSeparateAudio = true
	ch.CurrentFilename = base
	ch.InitSegment = videoMP4
	ch.AudioInitSegment = audioMP4

	if err := ch.CreateNewFile(base); err != nil {
		t.Fatalf("CreateNewFile() error = %v", err)
	}
	markSeparateTrackMediaWritten(ch, videoMP4, audioMP4)

	if err := ch.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	outputPath := base + ".mp4"
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatalf("expected final mp4, stat err = %v", err)
	}
	if info.Size() <= 0 {
		t.Fatalf("expected final mp4 to be non-empty, size = %d", info.Size())
	}
	if _, err := os.Stat(base + ".video.mp4"); !os.IsNotExist(err) {
		t.Fatalf("expected video sidecar removed, stat err = %v", err)
	}
	if _, err := os.Stat(base + ".audio.mp4"); !os.IsNotExist(err) {
		t.Fatalf("expected audio sidecar removed, stat err = %v", err)
	}

	// Verify the muxed output contains both video and audio tracks
	muxed, err := mp4.ReadMP4File(outputPath)
	if err != nil {
		t.Fatalf("ReadMP4File() error = %v", err)
	}
	if len(muxed.Init.Moov.Traks) != 2 {
		t.Fatalf("expected 2 tracks in muxed output, got %d", len(muxed.Init.Moov.Traks))
	}
}

func TestCleanupDropsInitOnlySeparateTrackFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	base := filepath.Join(dir, "recording")
	ch := New(&entity.ChannelConfig{
		Username: "alice",
		Pattern:  base,
	})
	ch.HasSeparateAudio = true
	ch.CurrentFilename = base
	ch.InitSegment = []byte("video-init-only")
	ch.AudioInitSegment = []byte("audio-init-only")

	if err := ch.CreateNewFile(base); err != nil {
		t.Fatalf("CreateNewFile() error = %v", err)
	}

	if err := ch.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	if _, err := os.Stat(base + ".mp4"); !os.IsNotExist(err) {
		t.Fatalf("expected no muxed output for init-only files, stat err = %v", err)
	}
	if _, err := os.Stat(base + ".video.mp4"); !os.IsNotExist(err) {
		t.Fatalf("expected init-only video sidecar removed, stat err = %v", err)
	}
	if _, err := os.Stat(base + ".audio.mp4"); !os.IsNotExist(err) {
		t.Fatalf("expected init-only audio sidecar removed, stat err = %v", err)
	}
}

func TestCleanupPreservesUncountedPartialMediaBytes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	base := filepath.Join(dir, "recording")
	audioPath := base + ".audio.mp4"
	audioInit := []byte("audio-init")
	audioFile, err := os.OpenFile(audioPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open audio file: %v", err)
	}
	if _, err := audioFile.Write(append([]byte{}, audioInit...)); err != nil {
		t.Fatalf("write audio init: %v", err)
	}
	if _, err := audioFile.Write([]byte("partial-media")); err != nil {
		t.Fatalf("write audio media: %v", err)
	}

	ch := New(&entity.ChannelConfig{Username: "alice", Pattern: base})
	ch.HasSeparateAudio = true
	ch.CurrentFilename = base
	ch.AudioInitSegment = audioInit
	ch.AudioFile = audioFile

	if err := ch.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if _, err := os.Stat(audioPath); err != nil {
		t.Fatalf("audio file with uncounted media bytes should be preserved, stat err = %v", err)
	}
}

func TestCreateNewFileKeepsLegacyHLSAsTS(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	base := filepath.Join(dir, "legacy")
	ch := New(&entity.ChannelConfig{
		Username: "alice",
		Pattern:  base,
	})

	if err := ch.CreateNewFile(base); err != nil {
		t.Fatalf("CreateNewFile() error = %v", err)
	}
	t.Cleanup(func() { _ = ch.Cleanup() })

	if _, err := os.Stat(base + ".ts"); err != nil {
		t.Fatalf("expected legacy ts output, stat err = %v", err)
	}
	if _, err := os.Stat(base + ".mp4"); !os.IsNotExist(err) {
		t.Fatalf("expected legacy mp4 output to not exist, stat err = %v", err)
	}
}

func TestHandleSegmentDefersRotationForSeparateAudio(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pattern := filepath.Join(dir, "rotating{{if .Sequence}}_{{.Sequence}}{{end}}")
	ch := New(&entity.ChannelConfig{
		Username:    "alice",
		Pattern:     pattern,
		MaxFilesize: 1, // 1 MiB threshold
	})
	ch.StreamedAt = 1
	ch.HasSeparateAudio = true

	if err := ch.NextFile(); err != nil {
		t.Fatalf("NextFile() error = %v", err)
	}
	t.Cleanup(func() { _ = ch.Cleanup() })

	firstName := ch.File.Name()

	// Trigger ShouldSwitchFile by writing past MaxFilesize.
	if err := ch.HandleSegment(make([]byte, 2*1024*1024), 1); err != nil {
		t.Fatalf("HandleSegment() error = %v", err)
	}

	if !ch.switchRequested {
		t.Fatalf("switchRequested = false after oversized write, want true")
	}
	if ch.File.Name() != firstName {
		t.Fatalf("file rotated inside HandleSegment: %q -> %q", firstName, ch.File.Name())
	}

	if err := ch.OnPollComplete(); err != nil {
		t.Fatalf("OnPollComplete() error = %v", err)
	}
	if ch.switchRequested {
		t.Fatalf("switchRequested still set after OnPollComplete")
	}
	if ch.File.Name() == firstName {
		t.Fatalf("file not rotated after OnPollComplete (still %q)", firstName)
	}
}

func TestHandleSegmentRotatesImmediatelyForSingleStream(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pattern := filepath.Join(dir, "single{{if .Sequence}}_{{.Sequence}}{{end}}")
	ch := New(&entity.ChannelConfig{
		Username:    "alice",
		Pattern:     pattern,
		MaxFilesize: 1, // 1 MiB threshold
	})
	ch.StreamedAt = 1
	// HasSeparateAudio stays false: no audio playlist, no pairing risk.

	if err := ch.NextFile(); err != nil {
		t.Fatalf("NextFile() error = %v", err)
	}
	t.Cleanup(func() { _ = ch.Cleanup() })

	firstName := ch.File.Name()

	if err := ch.HandleSegment(make([]byte, 2*1024*1024), 1); err != nil {
		t.Fatalf("HandleSegment() error = %v", err)
	}

	if ch.switchRequested {
		t.Fatalf("switchRequested = true for single-stream recording, want false")
	}
	if ch.File.Name() == firstName {
		t.Fatalf("file not rotated immediately for single-stream recording (still %q)", firstName)
	}
}

func TestOnPollCompleteNoopWhenNothingRequested(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ch := New(&entity.ChannelConfig{
		Username: "alice",
		Pattern:  filepath.Join(dir, "x"),
	})

	if err := ch.OnPollComplete(); err != nil {
		t.Fatalf("OnPollComplete() with no flag error = %v", err)
	}
}

func TestCleanupPreservesAudioOnlyWhenVideoMissing(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "recording")
	audioPath := base + ".audio.mp4"
	audioFile, err := os.OpenFile(audioPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open audio file: %v", err)
	}
	if _, err := audioFile.Write([]byte("audio-payload")); err != nil {
		t.Fatalf("write audio file: %v", err)
	}

	ch := New(&entity.ChannelConfig{Username: "alice", Pattern: base})
	ch.HasSeparateAudio = true
	ch.CurrentFilename = base
	ch.AudioFile = audioFile
	ch.audioMediaBytes = len("audio-payload")

	if err := ch.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if _, err := os.Stat(audioPath); err != nil {
		t.Fatalf("audio file should be preserved, stat err = %v", err)
	}
}

func TestCleanupPreservesVideoOnlyWhenAudioMissing(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "recording")
	videoPath := base + ".video.mp4"
	videoFile, err := os.OpenFile(videoPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open video file: %v", err)
	}
	if _, err := videoFile.Write([]byte("video-payload")); err != nil {
		t.Fatalf("write video file: %v", err)
	}

	ch := New(&entity.ChannelConfig{Username: "alice", Pattern: base})
	ch.HasSeparateAudio = true
	ch.CurrentFilename = base
	ch.File = videoFile
	ch.videoMediaBytes = len("video-payload")

	if err := ch.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if _, err := os.Stat(videoPath); err != nil {
		t.Fatalf("video file should be preserved, stat err = %v", err)
	}
}

func TestMuxOutputLooksValid(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	writeSized := func(name string, size int) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	videoPath := writeSized("v", 1000)
	audioPath := writeSized("a", 200)
	videoInfo, _ := os.Stat(videoPath)
	audioInfo, _ := os.Stat(audioPath)

	okOutput := writeSized("ok.mp4", 900) // 900 >= (1200 / 2)
	tinyOutput := writeSized("tiny.mp4", 100)
	emptyOutput := writeSized("empty.mp4", 0)

	if ok, reason := muxOutputLooksValid(okOutput, videoInfo, audioInfo); !ok {
		t.Fatalf("expected valid, got reason %q", reason)
	}
	if ok, _ := muxOutputLooksValid(tinyOutput, videoInfo, audioInfo); ok {
		t.Fatalf("expected invalid for tiny output")
	}
	if ok, _ := muxOutputLooksValid(emptyOutput, videoInfo, audioInfo); ok {
		t.Fatalf("expected invalid for empty output")
	}
	if ok, _ := muxOutputLooksValid(filepath.Join(dir, "missing.mp4"), videoInfo, audioInfo); ok {
		t.Fatalf("expected invalid for missing output")
	}
}

func TestCompressFilePreservesInputTiming(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ffmpeg.log")
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
last=""
for arg in "$@"; do
  last="$arg"
done
case "$last" in
  *.mkv) printf 'compressed' > "$last" ;;
esac
`, logPath)
	if err := os.WriteFile(ffmpegPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	// Stub ffprobe to report aligned streams so this test only checks the
	// timing-preservation flags. Offset trimming is exercised separately.
	ffprobePath := filepath.Join(dir, "ffprobe")
	if err := os.WriteFile(ffprobePath, []byte("#!/bin/sh\necho 0.000000\n"), 0755); err != nil {
		t.Fatalf("write fake ffprobe: %v", err)
	}
	t.Setenv("PATH", dir)

	oldDetectedEncoder := detectedEncoder
	oldFpsPassthroughFlag := append([]string(nil), fpsPassthroughFlag...)
	t.Cleanup(func() {
		detectedEncoder = oldDetectedEncoder
		fpsPassthroughFlag = oldFpsPassthroughFlag
		detectedEncoderOnce = sync.Once{}
		fpsPassthroughOnce = sync.Once{}
	})

	detectedEncoder = ""
	detectedEncoderOnce = sync.Once{}
	fpsPassthroughFlag = nil
	fpsPassthroughOnce = sync.Once{}

	srcPath := filepath.Join(dir, "recording.mp4")
	if err := os.WriteFile(srcPath, []byte("source-video"), 0644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	ch := New(&entity.ChannelConfig{Username: "alice", Pattern: filepath.Join(dir, "recording")})
	ch.CompressFile(srcPath)

	var log string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(logPath)
		if err == nil {
			log = string(data)
			if strings.Contains(log, ".mkv") {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(log, ".mkv") {
		t.Fatalf("compress ffmpeg command did not run, log = %q", log)
	}

	lines := strings.Split(strings.TrimSpace(log), "\n")
	compressArgs := lines[len(lines)-1]
	for _, want := range []string{"-copyts", "-start_at_zero"} {
		if !strings.Contains(compressArgs, want) {
			t.Fatalf("compress args = %q, want %q to preserve input timing", compressArgs, want)
		}
	}
	// Accept either modern (-fps_mode passthrough) or legacy (-vsync
	// passthrough) frame-timing flag, since the chosen one depends on the
	// installed ffmpeg version.
	if !strings.Contains(compressArgs, "-fps_mode passthrough") &&
		!strings.Contains(compressArgs, "-vsync passthrough") {
		t.Fatalf("compress args = %q, want -fps_mode or -vsync passthrough", compressArgs)
	}
	// Aligned streams (ffprobe stub returns 0) must not insert -ss.
	if strings.Contains(compressArgs, "-ss ") {
		t.Fatalf("compress args = %q, did not expect -ss when streams are aligned", compressArgs)
	}
}

func TestBuildCompressArgsAddsLeadingTrim(t *testing.T) {
	t.Parallel()

	enc := videoEncoder{name: "CPU", codec: "libx264", args: []string{"-preset", "medium", "-crf", "23"}}
	fps := []string{"-fps_mode", "passthrough"}

	aligned := buildCompressArgs("/in.mp4", "/out.mkv", enc, fps, 0)
	if containsArg(aligned, "-ss") {
		t.Fatalf("aligned compress args contain -ss: %v", aligned)
	}

	misaligned := buildCompressArgs("/in.mp4", "/out.mkv", enc, fps, 1.246)
	idx := indexOfArg(misaligned, "-ss")
	if idx < 0 {
		t.Fatalf("misaligned compress args missing -ss: %v", misaligned)
	}
	if got := misaligned[idx+1]; got != "1.246" {
		t.Fatalf("-ss value = %q, want 1.246", got)
	}
	// -ss must precede -i so it applies as input-side seek.
	if iIdx := indexOfArg(misaligned, "-i"); iIdx <= idx {
		t.Fatalf("-ss (%d) must come before -i (%d): %v", idx, iIdx, misaligned)
	}

	// Sub-threshold offsets are ignored to avoid trimming sub-frame jitter.
	jitter := buildCompressArgs("/in.mp4", "/out.mkv", enc, fps, 0.02)
	if containsArg(jitter, "-ss") {
		t.Fatalf("sub-threshold offset triggered -ss: %v", jitter)
	}
}

func TestDetectStreamStartOffsetSecWithFakeFFprobe(t *testing.T) {
	dir := t.TempDir()
	ffprobe := filepath.Join(dir, "ffprobe")
	// Fake ffprobe reads the desired video/audio start times from env vars
	// so individual sub-cases can drive different scenarios.
	script := `#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    "v:0") echo "$FAKE_V_START" ; exit 0 ;;
    "a:0") echo "$FAKE_A_START" ; exit 0 ;;
  esac
done
exit 1
`
	if err := os.WriteFile(ffprobe, []byte(script), 0755); err != nil {
		t.Fatalf("write fake ffprobe: %v", err)
	}
	t.Setenv("PATH", dir)

	cases := []struct {
		name      string
		v, a      string
		want      float64
		wantTrim  bool
		tolerance float64
	}{
		{name: "video leads", v: "0.000000", a: "1.246000", want: 1.246, wantTrim: true, tolerance: 0.001},
		{name: "audio leads", v: "1.246000", a: "0.000000", want: 1.246, wantTrim: true, tolerance: 0.001},
		{name: "aligned at zero", v: "0.000000", a: "0.000000", want: 0, wantTrim: false},
		{name: "aligned at non-zero baseline", v: "1.500000", a: "1.500000", want: 0, wantTrim: false},
		{name: "offset from non-zero baseline", v: "120.000000", a: "121.000000", want: 1.000, wantTrim: true, tolerance: 0.001},
		{name: "near-zero jitter ignored", v: "0.000000", a: "0.020000", want: 0, wantTrim: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FAKE_V_START", tc.v)
			t.Setenv("FAKE_A_START", tc.a)
			got := detectStreamStartOffsetSec(filepath.Join(dir, "irrelevant.mp4"))
			if !tc.wantTrim {
				if got != 0 {
					t.Fatalf("got skip = %v, want 0 (no trim)", got)
				}
				return
			}
			if got < tc.want-tc.tolerance || got > tc.want+tc.tolerance {
				t.Fatalf("got skip = %v, want %v ±%v", got, tc.want, tc.tolerance)
			}
		})
	}

	// Probe failure (ffprobe missing) returns 0 without panicking.
	t.Setenv("PATH", t.TempDir())
	if got := detectStreamStartOffsetSec("/nope.mp4"); got != 0 {
		t.Fatalf("expected 0 when ffprobe missing, got %v", got)
	}
}

func TestNativeMuxPromotesVersionForLongDurations(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	// Fragment Dur = timescale = 1e9 ticks (1 second wallclock per fragment).
	// Five fragments yields 5e9 mdhd ticks per track, which exceeds the
	// version-0 32-bit Duration limit (~4.29e9). Without version promotion
	// mp4ff would truncate the encoded duration silently.
	const hugeTimescale uint32 = 1_000_000_000
	videoMP4 := buildFragmentedMP4WithSamples(t, "video", hugeTimescale, []byte{0x00, 0x00, 0x00, 0x01, 0x67}, 5)
	audioMP4 := buildFragmentedMP4WithSamples(t, "audio", hugeTimescale, []byte{0xFF, 0xF1}, 5)

	base := filepath.Join(dir, "long")
	ch := New(&entity.ChannelConfig{Username: "alice", Pattern: base})
	ch.HasSeparateAudio = true
	ch.CurrentFilename = base
	ch.InitSegment = videoMP4
	ch.AudioInitSegment = audioMP4

	if err := ch.CreateNewFile(base); err != nil {
		t.Fatalf("CreateNewFile() error = %v", err)
	}
	markSeparateTrackMediaWritten(ch, videoMP4, audioMP4)
	if err := ch.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	muxed, err := mp4.ReadMP4File(base + ".mp4")
	if err != nil {
		t.Fatalf("ReadMP4File() error = %v", err)
	}

	mdhdLimit := uint64(0xFFFFFFFF)
	for _, trak := range muxed.Init.Moov.Traks {
		mdhd := trak.Mdia.Mdhd
		if mdhd.Duration > mdhdLimit && mdhd.Version == 0 {
			t.Fatalf("track %d mdhd has version 0 with duration %d (overflows uint32)", trak.Tkhd.TrackID, mdhd.Duration)
		}
		if trak.Tkhd.Duration > mdhdLimit && trak.Tkhd.Version == 0 {
			t.Fatalf("track %d tkhd has version 0 with duration %d (overflows uint32)", trak.Tkhd.TrackID, trak.Tkhd.Duration)
		}
	}
	mvhd := muxed.Init.Moov.Mvhd
	if mvhd.Duration > mdhdLimit && mvhd.Version == 0 {
		t.Fatalf("mvhd has version 0 with duration %d (overflows uint32)", mvhd.Duration)
	}
	// And the durations must reflect the input length, not a wrapped value.
	expectedSeconds := 5.0
	gotSeconds := float64(mvhd.Duration) / float64(mvhd.Timescale)
	if gotSeconds < expectedSeconds-0.5 || gotSeconds > expectedSeconds+0.5 {
		t.Fatalf("mvhd reports %.2fs, want ~%vs", gotSeconds, expectedSeconds)
	}
}

func containsArg(args []string, target string) bool {
	return indexOfArg(args, target) >= 0
}

func indexOfArg(args []string, target string) int {
	for i, a := range args {
		if a == target {
			return i
		}
	}
	return -1
}

func TestNativeMuxWritesNonZeroDuration(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	const fragments = 5
	videoMP4 := buildFragmentedMP4WithSamples(t, "video", 90000, []byte{0x00, 0x00, 0x00, 0x01, 0x67}, fragments)
	audioMP4 := buildFragmentedMP4WithSamples(t, "audio", 44100, []byte{0xFF, 0xF1}, fragments)

	base := filepath.Join(dir, "recording")
	ch := New(&entity.ChannelConfig{
		Username: "alice",
		Pattern:  base,
	})
	ch.HasSeparateAudio = true
	ch.CurrentFilename = base
	ch.InitSegment = videoMP4
	ch.AudioInitSegment = audioMP4

	if err := ch.CreateNewFile(base); err != nil {
		t.Fatalf("CreateNewFile() error = %v", err)
	}
	markSeparateTrackMediaWritten(ch, videoMP4, audioMP4)
	if err := ch.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	muxed, err := mp4.ReadMP4File(base + ".mp4")
	if err != nil {
		t.Fatalf("ReadMP4File() error = %v", err)
	}
	mvhd := muxed.Init.Moov.Mvhd
	if mvhd.Duration == 0 {
		t.Fatalf("expected mvhd.Duration > 0 to advertise recorded length, got 0 (timescale=%d)", mvhd.Timescale)
	}
	gotSeconds := float64(mvhd.Duration) / float64(mvhd.Timescale)
	if gotSeconds < float64(fragments)-0.5 || gotSeconds > float64(fragments)+0.5 {
		t.Fatalf("mvhd reports %.2fs, want ~%ds", gotSeconds, fragments)
	}

	for _, trak := range muxed.Init.Moov.Traks {
		if trak.Tkhd.Duration == 0 {
			t.Fatalf("track %d tkhd.Duration is zero", trak.Tkhd.TrackID)
		}
		if trak.Mdia == nil || trak.Mdia.Mdhd == nil {
			t.Fatalf("track %d missing mdhd", trak.Tkhd.TrackID)
		}
		mdhd := trak.Mdia.Mdhd
		if mdhd.Duration == 0 {
			t.Fatalf("track %d mdhd.Duration is zero", trak.Tkhd.TrackID)
		}
		mediaSeconds := float64(mdhd.Duration) / float64(mdhd.Timescale)
		if mediaSeconds < float64(fragments)-0.5 || mediaSeconds > float64(fragments)+0.5 {
			t.Fatalf("track %d mdhd reports %.2fs, want ~%ds", trak.Tkhd.TrackID, mediaSeconds, fragments)
		}
	}
}
