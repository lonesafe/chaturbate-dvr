package channel

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Eyevinn/mp4ff/bits"
	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/teacat/chaturbate-dvr/internal"
)

type realtimeMuxer struct {
	videoTrex       *mp4.TrexBox
	audioTrex       *mp4.TrexBox
	videoTimescale  uint32
	audioTimescale  uint32
	videoTimeOffset uint64
	audioTimeOffset uint64
	timelineRebased bool
	initData        []byte
}

func newRealtimeMuxer(videoData, audioData []byte) (*realtimeMuxer, error) {
	videoFile, err := mp4.DecodeFileSR(bits.NewFixedSliceReader(videoData))
	if err != nil {
		return nil, fmt.Errorf("解析视频初始化信息：%w", err)
	}
	audioFile, err := mp4.DecodeFileSR(bits.NewFixedSliceReader(audioData))
	if err != nil {
		return nil, fmt.Errorf("解析音频初始化信息：%w", err)
	}
	videoTrack, videoTrex, err := sourceTrack(videoFile, "vide")
	if err != nil {
		return nil, err
	}
	audioTrack, audioTrex, err := sourceTrack(audioFile, "soun")
	if err != nil {
		return nil, err
	}
	videoTimescale := videoTrack.Mdia.Mdhd.Timescale
	audioTimescale := audioTrack.Mdia.Mdhd.Timescale
	if videoTimescale == 0 || audioTimescale == 0 {
		return nil, fmt.Errorf("轨道时间刻度无效（视频=%d，音频=%d）", videoTimescale, audioTimescale)
	}
	// Keep source trex metadata unchanged for parsing incoming single-track
	// fragments; the combined init uses separate mutated copies.
	videoSourceTrex := *videoTrex
	audioSourceTrex := *audioTrex
	videoTrack.Tkhd.TrackID = videoTrackID
	videoTrex.TrackID = videoTrackID
	audioTrack.Tkhd.TrackID = audioTrackID
	audioTrex.TrackID = audioTrackID
	videoFile.Init.Moov.AddChild(audioTrack)
	videoFile.Init.Moov.Mvex.AddChild(audioTrex)
	videoFile.Init.Moov.Mvhd.NextTrackID = audioTrackID + 1
	var output bytes.Buffer
	if err := videoFile.Init.Encode(&output); err != nil {
		return nil, fmt.Errorf("编码双轨初始化信息：%w", err)
	}
	return &realtimeMuxer{
		videoTrex:      &videoSourceTrex,
		audioTrex:      &audioSourceTrex,
		videoTimescale: videoTimescale,
		audioTimescale: audioTimescale,
		initData:       output.Bytes(),
	}, nil
}

func (m *realtimeMuxer) combineMedia(videoData, audioData []byte, sequence uint32) ([]byte, error) {
	return m.combineMediaGroup([][]byte{videoData}, [][]byte{audioData}, sequence)
}

func (m *realtimeMuxer) combineMediaGroup(videoData, audioData [][]byte, sequence uint32) ([]byte, error) {
	videoFragments := make([]*mp4.Fragment, 0)
	for _, data := range videoData {
		videoFile, err := mp4.DecodeFileSR(bits.NewFixedSliceReader(data))
		if err != nil {
			return nil, fmt.Errorf("解析视频媒体分片：%w", err)
		}
		videoFragments = append(videoFragments, collectFragments(videoFile)...)
	}
	audioFragments := make([]*mp4.Fragment, 0)
	for _, data := range audioData {
		audioFile, err := mp4.DecodeFileSR(bits.NewFixedSliceReader(data))
		if err != nil {
			return nil, fmt.Errorf("解析音频媒体分片：%w", err)
		}
		audioFragments = append(audioFragments, collectFragments(audioFile)...)
	}
	return m.combineDecodedFragments(videoFragments, audioFragments, sequence)
}

func (m *realtimeMuxer) combineDecodedFragments(videoFragments, audioFragments []*mp4.Fragment, sequence uint32) ([]byte, error) {
	if len(videoFragments) == 0 || len(audioFragments) == 0 {
		return nil, fmt.Errorf("缺少音频或视频分片")
	}
	if err := m.initializeTimelineOffsets(videoFragments, audioFragments); err != nil {
		return nil, err
	}
	fragment, err := mp4.CreateMultiTrackFragment(sequence, []uint32{videoTrackID, audioTrackID})
	if err != nil {
		return nil, err
	}
	for _, source := range videoFragments {
		if err := appendFragmentSamplesWithOffset(fragment, source, m.videoTrex, videoTrackID, m.videoTimeOffset); err != nil {
			return nil, fmt.Errorf("追加视频采样：%w", err)
		}
	}
	for _, source := range audioFragments {
		if err := appendFragmentSamplesWithOffset(fragment, source, m.audioTrex, audioTrackID, m.audioTimeOffset); err != nil {
			return nil, fmt.Errorf("追加音频采样：%w", err)
		}
	}
	segment := mp4.NewMediaSegmentWithoutStyp()
	segment.AddFragment(fragment)
	var output bytes.Buffer
	if err := segment.Encode(&output); err != nil {
		return nil, fmt.Errorf("编码双轨媒体分片：%w", err)
	}
	return output.Bytes(), nil
}

// initializeTimelineOffsets maps the source stream's long-running absolute
// TFDT clock onto a recording-local timeline. Both tracks use the same wall
// clock baseline, so their original A/V offset is preserved while the first
// output sample starts at (or within one timescale tick of) zero.
func (m *realtimeMuxer) initializeTimelineOffsets(videoFragments, audioFragments []*mp4.Fragment) error {
	if m.timelineRebased {
		return nil
	}
	videoStart, err := firstFragmentDecodeTime(videoFragments, m.videoTrex)
	if err != nil {
		return fmt.Errorf("读取视频时间轴起点：%w", err)
	}
	audioStart, err := firstFragmentDecodeTime(audioFragments, m.audioTrex)
	if err != nil {
		return fmt.Errorf("读取音频时间轴起点：%w", err)
	}

	videoSeconds := float64(videoStart) / float64(m.videoTimescale)
	audioSeconds := float64(audioStart) / float64(m.audioTimescale)
	baselineSeconds := min(videoSeconds, audioSeconds)
	m.videoTimeOffset = uint64(baselineSeconds * float64(m.videoTimescale))
	m.audioTimeOffset = uint64(baselineSeconds * float64(m.audioTimescale))
	// Floating point rounding must never move a track before its first sample.
	m.videoTimeOffset = min(m.videoTimeOffset, videoStart)
	m.audioTimeOffset = min(m.audioTimeOffset, audioStart)
	m.timelineRebased = true
	return nil
}

func firstFragmentDecodeTime(fragments []*mp4.Fragment, trex *mp4.TrexBox) (uint64, error) {
	for _, fragment := range fragments {
		if fragment == nil {
			continue
		}
		samples, err := fragment.GetFullSamples(trex)
		if err != nil {
			return 0, err
		}
		if len(samples) > 0 {
			return samples[0].DecodeTime, nil
		}
	}
	return 0, fmt.Errorf("缺少采样数据")
}

func combineInitSegments(videoData, audioData []byte) ([]byte, error) {
	muxer, err := newRealtimeMuxer(videoData, audioData)
	if err != nil {
		return nil, err
	}
	return muxer.initData, nil
}

func combineMediaSegments(videoInit, audioInit, videoData, audioData []byte, sequence uint32) ([]byte, error) {
	muxer, err := newRealtimeMuxer(videoInit, audioInit)
	if err != nil {
		return nil, err
	}
	return muxer.combineMedia(videoData, audioData, sequence)
}

// Track IDs for muxed output
const (
	videoTrackID uint32 = 1
	audioTrackID uint32 = 2
)

const ffmpegProbeTimeout = 5 * time.Second

const (
	realtimeFinalizeTimeout     = 30 * time.Minute
	realtimeFinalizeConcurrency = 2
	realtimeFinalizeHeadroom    = 64 << 20
)

var realtimeFinalizeSlots = make(chan struct{}, realtimeFinalizeConcurrency)

// GPU encoder detection cache
var (
	detectedEncoder     string
	detectedEncoderOnce sync.Once
)

// Frame-timing flag detection cache. ffmpeg 5+ uses -fps_mode (per-stream);
// 4.x only knows the legacy global -vsync. Probing once at runtime keeps
// CompressFile working on either generation without a static dependency.
var (
	fpsPassthroughFlag []string
	fpsPassthroughOnce sync.Once
)

// videoEncoder represents a video encoder configuration
type videoEncoder struct {
	name  string   // display name
	codec string   // ffmpeg codec name
	args  []string // additional encoder arguments
}

// availableEncoders lists GPU encoders in priority order, with CPU fallback last
var availableEncoders = []videoEncoder{
	// NVIDIA NVENC - use higher cq value for better compression (scale is 0-51, higher = smaller file)
	{"NVENC", "h264_nvenc", []string{"-preset", "p4", "-rc", "vbr", "-cq", "30", "-b:v", "0"}},
	// AMD AMF
	{"AMF", "h264_amf", []string{"-quality", "balanced", "-rc", "vbr_latency", "-qp_i", "28", "-qp_p", "28"}},
	// Intel Quick Sync
	{"QSV", "h264_qsv", []string{"-preset", "medium", "-global_quality", "28"}},
	// macOS VideoToolbox
	{"VideoToolbox", "h264_videotoolbox", []string{"-q:v", "65"}},
	// CPU fallback
	{"CPU", "libx264", []string{"-preset", "medium", "-crf", "23"}},
}

// detectEncoder finds the best available encoder
func detectEncoder() (videoEncoder, string) {
	for _, enc := range availableEncoders {
		// Test if encoder is available by running ffmpeg with it
		cmd := exec.Command("ffmpeg", "-hide_banner", "-f", "lavfi", "-i", "nullsrc=s=256x256:d=1", "-c:v", enc.codec, "-f", "null", "-")
		if err := cmd.Run(); err == nil {
			return enc, enc.name
		}
	}
	// Should not reach here since libx264 is always available if ffmpeg is installed
	return availableEncoders[len(availableEncoders)-1], "CPU"
}

// getFpsPassthroughFlag returns the ffmpeg flag(s) that pass each video
// frame's timestamp straight through to the muxer. Modern ffmpeg uses
// -fps_mode passthrough; older 4.x exits with "Unrecognized option
// 'fps_mode'", so we probe support once and fall back to -vsync passthrough
// (deprecated on 5+, but still functional through ffmpeg 8).
func getFpsPassthroughFlag() []string {
	fpsPassthroughOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), ffmpegProbeTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
			"-f", "lavfi", "-i", "nullsrc=s=64x64:d=0.05",
			"-fps_mode", "passthrough", "-f", "null", "-")
		if err := cmd.Run(); err == nil {
			fpsPassthroughFlag = []string{"-fps_mode", "passthrough"}
		} else {
			fpsPassthroughFlag = []string{"-vsync", "passthrough"}
		}
	})
	return fpsPassthroughFlag
}

// detectStreamStartOffsetSec returns the seconds that should be skipped from
// the input so the muxed video and audio first samples line up at output time
// zero. LL-HLS video and audio playlists are independent, so the first
// fetched fragment from each rarely covers the same wall-clock moment; the
// later stream's first sample sits at output PTS=delta with the earlier
// stream filling delta seconds of leading silence/black, which the user
// reads as "A/V is off". Probing the muxed input lets the compress step skip
// that leading mismatch while re-encoding (where -ss is sample-accurate).
//
// Returns 0 when probing fails, ffprobe is unavailable, or the offset is
// below alignTrimThreshold (no point trimming sub-frame jitter).
func detectStreamStartOffsetSec(srcPath string) float64 {
	probe := func(stream string) (float64, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), ffmpegProbeTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error",
			"-select_streams", stream,
			"-show_entries", "stream=start_time",
			"-of", "csv=p=0", srcPath)
		out, err := cmd.Output()
		if err != nil {
			return 0, false
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	videoStart, vOK := probe("v:0")
	audioStart, aOK := probe("a:0")
	if !vOK || !aOK {
		return 0
	}
	// Trim is only justified when the two streams disagree about where they
	// start. If both share the same baseline (e.g. both at 1.5s on inputs
	// produced by other tools), -ss would chop real aligned content even
	// though there is nothing to fix. Compare the offset between streams,
	// not the absolute baseline.
	diff := videoStart - audioStart
	if diff < 0 {
		diff = -diff
	}
	if diff < alignTrimThreshold {
		return 0
	}
	return diff
}

// alignTrimThreshold is the minimum mismatch (seconds) before we bother
// trimming. Anything below this is sub-frame jitter that ffmpeg's existing
// -avoid_negative_ts already handles cleanly.
const alignTrimThreshold = 0.05

// buildCompressArgs assembles the ffmpeg command line for compress, isolated
// for testability. skipSec, when above the threshold, becomes an input-side
// -ss so the leading misaligned segment is dropped accurately by the
// re-encoder.
func buildCompressArgs(srcPath, mkvPath string, encoder videoEncoder, fpsFlag []string, skipSec float64) []string {
	args := []string{"-y", "-copyts", "-start_at_zero"}
	if skipSec >= alignTrimThreshold {
		args = append(args, "-ss", strconv.FormatFloat(skipSec, 'f', 3, 64))
	}
	args = append(args, "-i", srcPath, "-c:v", encoder.codec)
	args = append(args, encoder.args...)
	args = append(args, fpsFlag...)
	args = append(args, "-c:a", "aac", "-b:a", "128k", "-avoid_negative_ts", "make_zero", mkvPath)
	return args
}

// getEncoder returns the cached encoder or detects one
func getEncoder() videoEncoder {
	detectedEncoderOnce.Do(func() {
		enc, name := detectEncoder()
		detectedEncoder = name
		_ = enc // stored via name lookup
	})

	for _, enc := range availableEncoders {
		if enc.name == detectedEncoder {
			return enc
		}
	}
	return availableEncoders[len(availableEncoders)-1]
}

// CompressFile compresses a video file (.ts or .mp4) to .mkv format using ffmpeg in the background.
// Uses hardware GPU encoding if available, falls back to CPU (libx264).
// After successful compression, the original file is deleted.
func (ch *Channel) CompressFile(srcPath string) {
	ch.compressWG.Add(1)
	go func() {
		defer ch.compressWG.Done()
		ext := filepath.Ext(srcPath)
		desiredMKVPath := strings.TrimSuffix(srcPath, ext) + ".mkv"
		temp, err := os.CreateTemp(filepath.Dir(srcPath), ".compress-*.mkv")
		if err != nil {
			ch.Error("压缩：创建临时输出文件失败：%s", err.Error())
			return
		}
		mkvPath := temp.Name()
		_ = temp.Close()
		defer os.Remove(mkvPath)
		srcFilename := filepath.Base(srcPath)

		// Get original file size
		srcInfo, err := os.Stat(srcPath)
		if err != nil {
			ch.Error("压缩：读取源文件信息失败：%s", err.Error())
			return
		}
		srcSize := srcInfo.Size()

		// Get the best available encoder
		encoder := getEncoder()

		// Detect any leading misalignment between the muxed video/audio so
		// the re-encoder can drop the leading silent gap. Probing happens
		// before the size log so the message reflects whatever portion will
		// actually end up in the output.
		skipSec := detectStreamStartOffsetSec(srcPath)
		if skipSec >= alignTrimThreshold {
			ch.Info("压缩：为修正音视频起始偏移，从 %s 跳过开头 %.3f 秒", srcFilename, skipSec)
		}

		ch.Info("压缩：正在使用 %s 编码 %s（%s）", encoder.name, srcFilename, internal.FormatFilesize(int(srcSize)))

		// Preserve the recorded timeline while re-encoding. LL-HLS/fMP4
		// inputs often carry variable frame timing; letting ffmpeg
		// normalize frame cadence can make audio drift during compression.
		args := buildCompressArgs(srcPath, mkvPath, encoder, getFpsPassthroughFlag(), skipSec)

		cmd := exec.Command("ffmpeg", args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			ch.Error("压缩：处理 %s 失败：%s", srcFilename, err.Error())
			if len(output) > 0 {
				// Only show last 500 chars of ffmpeg output to avoid flooding logs
				outStr := string(output)
				if len(outStr) > 500 {
					outStr = outStr[len(outStr)-500:]
				}
				ch.Error("压缩：ffmpeg 输出：%s", outStr)
			}
			return
		}

		// Get compressed file size
		mkvInfo, err := os.Stat(mkvPath)
		if err != nil {
			ch.Error("压缩：读取 MKV 文件信息失败：%s", err.Error())
			return
		}
		mkvSize := mkvInfo.Size()

		// Calculate compression ratio
		ratio := float64(mkvSize) / float64(srcSize) * 100

		finalMKVPath, err := moveFileUnique(mkvPath, desiredMKVPath)
		if err != nil {
			ch.Error("压缩：提交输出文件失败：%s", err.Error())
			return
		}
		// Delete the original file after successful compression
		if err := os.Remove(srcPath); err != nil {
			ch.Error("压缩：删除源文件 %s 失败：%s", srcFilename, err.Error())
			return
		}

		ch.Info("压缩完成：%s -> %s（%s，原文件的 %.1f%%）", srcFilename, filepath.Base(finalMKVPath), internal.FormatFilesize(int(mkvSize)), ratio)

		ch.MoveToOutputDir(finalMKVPath)
	}()
}

// MuxAV combines separate video and audio source files into a single MP4 container.
func (ch *Channel) MuxAV(videoPath, audioPath, outputPath string) error {
	// LL-HLS fragments are timestamped against an absolute presentation
	// timeline (TFDT), so the raw video and audio fragments only line up
	// if we preserve those timestamps with -copyts. Dropping -copyts made
	// ffmpeg renormalize each input to start at zero independently — which
	// is fine when the first fetched video/audio segments happened to
	// represent the same wall-clock moment, but when they differ (very
	// common on the very first poll of a live stream), the sound from the
	// later audio segment ends up playing against the earlier video
	// content, so users hear audio running seconds ahead of video.
	//
	// Keep -copyts for content alignment, -shortest so a stray partial
	// segment on one side cannot extend the combined duration past the
	// point both tracks have real samples, and -avoid_negative_ts
	// make_zero so H.264 B-frame reordering (negative DTS on the first
	// packet) cannot desync the output on strict players.
	args := []string{
		"-y",
		"-i", videoPath,
		"-i", audioPath,
		"-map", "0:v:0",
		"-map", "1:a:0",
		"-c", "copy",
		"-copyts",
		"-shortest",
		"-avoid_negative_ts", "make_zero",
		outputPath,
	}

	cmd := exec.Command("ffmpeg", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if len(output) > 0 {
			outStr := string(output)
			if len(outStr) > 500 {
				outStr = outStr[len(outStr)-500:]
			}
			ch.Error("合并：ffmpeg 输出：%s", outStr)
		}
		return fmt.Errorf("合并音视频：%w", err)
	}

	ch.Info("合并完成：%s + %s -> %s", filepath.Base(videoPath), filepath.Base(audioPath), filepath.Base(outputPath))
	return nil
}

// finalizeRealtimeRecording converts a completed fragmented MP4 into a
// regular fast-start MP4 without re-encoding. It runs after the recording
// file has been closed, and never removes the source until the validated
// output has been durably committed under its final name.
func (ch *Channel) finalizeRealtimeRecording(sourcePath, desiredPath string) {
	ch.compressWG.Add(1)
	go func() {
		defer ch.compressWG.Done()

		realtimeFinalizeSlots <- struct{}{}
		ch.Info("收尾：正在优化 %s，以便快速开始播放", filepath.Base(sourcePath))
		finalPath, err := optimizeRealtimeMP4(sourcePath, desiredPath)
		<-realtimeFinalizeSlots

		if err != nil {
			ch.Error("收尾：快速播放优化失败：%s；保留原始录像", err.Error())
			finalPath, err = moveFileUnique(sourcePath, desiredPath)
			if err != nil {
				ch.Error("收尾：无法提交原始录像：%s；可恢复文件仍保留在 %s", err.Error(), sourcePath)
				return
			}
		} else if err := os.Remove(sourcePath); err != nil && !os.IsNotExist(err) {
			// The optimized output is already committed. Leaving the source in
			// place is safer than treating a cleanup failure as output failure.
			ch.Error("收尾：优化文件已提交，但清理源文件失败：%s", err.Error())
		}

		ch.Info("收尾：已生成可快速播放的文件：%s", filepath.Base(finalPath))
		ch.MoveToOutputDir(finalPath)
	}()
}

func buildRealtimeFinalizeArgs(sourcePath, outputPath string) []string {
	return []string{
		"-hide_banner",
		"-loglevel", "error",
		"-nostdin",
		"-n",
		"-i", sourcePath,
		"-map", "0:v:0",
		"-map", "0:a:0",
		"-c", "copy",
		"-copyts",
		"-start_at_zero",
		"-avoid_negative_ts", "make_zero",
		"-movflags", "+faststart",
		outputPath,
	}
}

func optimizeRealtimeMP4(sourcePath, desiredPath string) (string, error) {
	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		return "", fmt.Errorf("读取源文件信息：%w", err)
	}
	if sourceInfo.Size() <= 0 {
		return "", fmt.Errorf("源文件为空")
	}

	free, err := availableDiskBytes(filepath.Dir(sourcePath))
	if err != nil {
		return "", fmt.Errorf("检查收尾处理所需磁盘空间：%w", err)
	}
	required := uint64(sourceInfo.Size()) + uint64(configuredMinimumFreeDiskBytes()) + realtimeFinalizeHeadroom
	if free < required {
		return "", fmt.Errorf("临时磁盘空间不足：包含预留空间共需 %s，当前可用 %s",
			internal.FormatFilesize(int(required)), internal.FormatFilesize(int(free)))
	}

	temp, err := os.CreateTemp(filepath.Dir(sourcePath), ".finalize-*.mp4")
	if err != nil {
		return "", fmt.Errorf("创建收尾临时文件：%w", err)
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return "", fmt.Errorf("关闭收尾临时文件：%w", err)
	}
	// ffmpeg -n requires a path that does not yet exist. The random path is
	// reserved only long enough to obtain a collision-resistant name.
	if err := os.Remove(tempPath); err != nil {
		return "", fmt.Errorf("准备收尾临时文件：%w", err)
	}
	defer os.Remove(tempPath)

	ctx, cancel := context.WithTimeout(context.Background(), realtimeFinalizeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", buildRealtimeFinalizeArgs(sourcePath, tempPath)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("ffmpeg 处理超时：%w", ctx.Err())
		}
		message := strings.TrimSpace(string(output))
		if len(message) > 500 {
			message = message[len(message)-500:]
		}
		if message != "" {
			return "", fmt.Errorf("ffmpeg 重新封装失败：%w：%s", err, message)
		}
		return "", fmt.Errorf("ffmpeg 重新封装失败：%w", err)
	}

	if err := validateOptimizedMP4(tempPath, sourceInfo.Size()); err != nil {
		return "", err
	}
	optimized, err := os.OpenFile(tempPath, os.O_RDONLY, 0)
	if err != nil {
		return "", fmt.Errorf("打开优化输出以同步：%w", err)
	}
	syncErr := optimized.Sync()
	closeErr := optimized.Close()
	if syncErr != nil {
		return "", fmt.Errorf("同步优化输出：%w", syncErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("关闭优化输出：%w", closeErr)
	}

	finalPath, err := moveFileUnique(tempPath, desiredPath)
	if err != nil {
		return "", fmt.Errorf("提交优化输出：%w", err)
	}
	return finalPath, nil
}

func validateOptimizedMP4(path string, sourceSize int64) error {
	layout, outputSize, err := inspectMP4File(path)
	if err != nil {
		return fmt.Errorf("检查优化输出：%w", err)
	}
	return validateOptimizedLayout(layout, outputSize, sourceSize)
}

func validateOptimizedLayout(layout mp4Layout, outputSize, sourceSize int64) error {
	if outputSize <= 0 || (sourceSize > 0 && outputSize < sourceSize/2) {
		return fmt.Errorf("优化输出大小异常：输出=%d，源文件=%d", outputSize, sourceSize)
	}
	if layout.ftypCount != 1 || layout.moovCount != 1 || !layout.hasMediaData || layout.hasFragments {
		return fmt.Errorf("优化输出不是常规索引 MP4")
	}
	if !layout.hasMovieHeader || layout.movieDuration == 0 {
		return fmt.Errorf("优化输出缺少影片时长")
	}
	if layout.trackCount != 2 {
		return fmt.Errorf("优化输出包含 %d 条轨道，应为 2 条", layout.trackCount)
	}
	if !layout.moovBeforeMediaData {
		return fmt.Errorf("优化输出不支持快速开始播放")
	}
	return nil
}

// MuxAVNative combines separate fragmented MP4 audio/video tracks without ffmpeg.
func (ch *Channel) MuxAVNative(videoPath, audioPath, outputPath string) error {
	videoFile, err := mp4.ReadMP4File(videoPath)
	if err != nil {
		return fmt.Errorf("解析视频 MP4：%w", err)
	}
	audioFile, err := mp4.ReadMP4File(audioPath)
	if err != nil {
		return fmt.Errorf("解析音频 MP4：%w", err)
	}

	outFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("创建音视频合并输出：%w", err)
	}
	defer outFile.Close()

	warn := func(msg string) { ch.Info("合并：%s", msg) }
	if err := writeCombinedFragmentedMP4(outFile, videoFile, audioFile, warn); err != nil {
		outFile.Close()
		os.Remove(outputPath)
		return fmt.Errorf("原生方式合并音视频：%w", err)
	}

	ch.Info("合并完成：%s + %s -> %s（原生方式）", filepath.Base(videoPath), filepath.Base(audioPath), filepath.Base(outputPath))
	return nil
}

func writeCombinedFragmentedMP4(w io.Writer, videoFile, audioFile *mp4.File, warn func(string)) error {
	_, videoTrex, err := sourceTrack(videoFile, "vide")
	if err != nil {
		return fmt.Errorf("加载视频轨：%w", err)
	}
	audioTrack, audioTrex, err := sourceTrack(audioFile, "soun")
	if err != nil {
		return fmt.Errorf("加载音频轨：%w", err)
	}

	// Combine fragments BEFORE reassigning track IDs — GetFullSamples
	// matches source traf boxes by trex.TrackID, which must still hold
	// the original value from the source file.
	videoFragments := collectFragments(videoFile)
	audioFragments := collectFragments(audioFile)
	if warn != nil && len(videoFragments) != len(audioFragments) {
		warn(fmt.Sprintf("音视频分片数量不一致（视频=%d，音频=%d）；输出末尾可能只有单条轨道", len(videoFragments), len(audioFragments)))
	}
	segments, err := combineTrackFragments(videoFragments, videoTrex, audioFragments, audioTrex)
	if err != nil {
		return err
	}

	// Compute total media-timescale duration from the source fragments
	// while track IDs still match the original trex values, so the duration
	// hints written into mvhd/tkhd/mdhd reflect the real recorded length.
	// Without this, mvhd.Duration stays 0 (the value from a live init
	// segment), and players that read it as a hint instead of scanning every
	// fragment report the recording as much shorter than it is.
	videoMediaDur := sumFragmentDurations(videoFragments, videoTrex)
	audioMediaDur := sumFragmentDurations(audioFragments, audioTrex)

	ftyp := videoFile.Init.Ftyp
	moov := videoFile.Init.Moov
	if len(moov.Traks) != 1 || moov.Mvex == nil || len(moov.Mvex.Trexs) != 1 {
		return fmt.Errorf("视频初始化信息应只包含一条轨道")
	}

	videoTrak := moov.Traks[0]
	if videoTrak.Mdia == nil || videoTrak.Mdia.Mdhd == nil {
		return fmt.Errorf("视频轨缺少 mdhd")
	}
	if audioTrack.Mdia == nil || audioTrack.Mdia.Mdhd == nil {
		return fmt.Errorf("音频轨缺少 mdhd")
	}

	videoTrak.Tkhd.TrackID = videoTrackID
	moov.Mvex.Trexs[0].TrackID = videoTrackID

	audioTrack.Tkhd.TrackID = audioTrackID
	audioTrex.TrackID = audioTrackID

	moov.AddChild(audioTrack)
	moov.Mvex.AddChild(audioTrex)
	moov.Mvhd.NextTrackID = audioTrackID + 1

	movieTimescale := uint64(moov.Mvhd.Timescale)
	if movieTimescale == 0 {
		movieTimescale = 1
	}
	videoMdhdTimescale := uint64(videoTrak.Mdia.Mdhd.Timescale)
	audioMdhdTimescale := uint64(audioTrack.Mdia.Mdhd.Timescale)

	videoTrak.Mdia.Mdhd.Duration = videoMediaDur
	promoteVersionForLongDuration(&videoTrak.Mdia.Mdhd.Version, videoMediaDur)
	audioTrack.Mdia.Mdhd.Duration = audioMediaDur
	promoteVersionForLongDuration(&audioTrack.Mdia.Mdhd.Version, audioMediaDur)

	videoMovieDur := scaleDuration(videoMediaDur, movieTimescale, videoMdhdTimescale)
	audioMovieDur := scaleDuration(audioMediaDur, movieTimescale, audioMdhdTimescale)
	videoTrak.Tkhd.Duration = videoMovieDur
	promoteVersionForLongDuration(&videoTrak.Tkhd.Version, videoMovieDur)
	audioTrack.Tkhd.Duration = audioMovieDur
	promoteVersionForLongDuration(&audioTrack.Tkhd.Version, audioMovieDur)

	moov.Mvhd.Duration = videoMovieDur
	if audioMovieDur > moov.Mvhd.Duration {
		moov.Mvhd.Duration = audioMovieDur
	}
	promoteVersionForLongDuration(&moov.Mvhd.Version, moov.Mvhd.Duration)

	out := mp4.NewFile()
	out.AddChild(ftyp, 0)
	out.AddChild(moov, ftyp.Size())
	for _, segment := range segments {
		out.AddMediaSegment(segment)
	}

	return out.Encode(w)
}

// sumFragmentDurations totals the trun durations of every fragment that
// belongs to the trex's track, falling back to the per-fragment or trex
// default sample duration when individual sample durations are absent.
func sumFragmentDurations(fragments []*mp4.Fragment, trex *mp4.TrexBox) uint64 {
	if trex == nil {
		return 0
	}
	var total uint64
	for _, frag := range fragments {
		if frag == nil || frag.Moof == nil {
			continue
		}
		for _, traf := range frag.Moof.Trafs {
			if traf == nil || traf.Tfhd == nil || traf.Tfhd.TrackID != trex.TrackID {
				continue
			}
			defaultDur := trex.DefaultSampleDuration
			if traf.Tfhd.HasDefaultSampleDuration() {
				defaultDur = traf.Tfhd.DefaultSampleDuration
			}
			for _, trun := range traf.Truns {
				if trun == nil {
					continue
				}
				total += trun.Duration(defaultDur)
			}
		}
	}
	return total
}

// scaleDuration converts duration from one timescale to another using
// integer math, guarding against zero divisors.
func scaleDuration(dur, dstTimescale, srcTimescale uint64) uint64 {
	if srcTimescale == 0 {
		return 0
	}
	return dur * dstTimescale / srcTimescale
}

// maxV0Duration is the largest value mp4ff can serialize into a version-0
// mvhd/tkhd/mdhd Duration field; the wire format uses uint32 there. At a
// 90 kHz video timescale this caps version-0 boxes at ~13.25 hours.
const maxV0Duration uint64 = 0xFFFFFFFF

// promoteVersionForLongDuration upgrades a header box from version 0 to
// version 1 when the duration to be encoded exceeds the 32-bit field used
// in the version-0 wire format. Without this, recordings longer than the
// version-0 limit would be silently truncated by mp4ff's encoder even
// though we set the uint64 Duration field to the correct value.
func promoteVersionForLongDuration(version *byte, duration uint64) {
	if version == nil {
		return
	}
	if duration > maxV0Duration && *version == 0 {
		*version = 1
	}
}

func sourceTrack(file *mp4.File, handlerType string) (*mp4.TrakBox, *mp4.TrexBox, error) {
	if file == nil || file.Init == nil || file.Init.Moov == nil {
		return nil, nil, fmt.Errorf("缺少初始化分片")
	}
	if len(file.Init.Moov.Traks) != 1 {
		return nil, nil, fmt.Errorf("应包含一条轨道，实际为 %d 条", len(file.Init.Moov.Traks))
	}

	trak := file.Init.Moov.Traks[0]
	if trak == nil || trak.Tkhd == nil || trak.Mdia == nil || trak.Mdia.Hdlr == nil {
		return nil, nil, fmt.Errorf("轨道元数据无效")
	}
	if trak.Mdia.Hdlr.HandlerType != handlerType {
		return nil, nil, fmt.Errorf("预期轨道类型为 %s，实际为 %s", handlerType, trak.Mdia.Hdlr.HandlerType)
	}
	if file.Init.Moov.Mvex == nil {
		return nil, nil, fmt.Errorf("缺少 mvex")
	}

	trex, ok := file.Init.Moov.Mvex.GetTrex(trak.Tkhd.TrackID)
	if !ok || trex == nil {
		return nil, nil, fmt.Errorf("轨道 %d 缺少 trex", trak.Tkhd.TrackID)
	}

	return trak, trex, nil
}

func combineTrackFragments(videoFragments []*mp4.Fragment, videoTrex *mp4.TrexBox, audioFragments []*mp4.Fragment, audioTrex *mp4.TrexBox) ([]*mp4.MediaSegment, error) {
	maxFragments := len(videoFragments)
	if len(audioFragments) > maxFragments {
		maxFragments = len(audioFragments)
	}
	if maxFragments == 0 {
		return nil, fmt.Errorf("缺少媒体分片")
	}

	segments := make([]*mp4.MediaSegment, 0, maxFragments)
	for i := 0; i < maxFragments; i++ {
		trackIDs := make([]uint32, 0, 2)
		if i < len(videoFragments) {
			trackIDs = append(trackIDs, videoTrackID)
		}
		if i < len(audioFragments) {
			trackIDs = append(trackIDs, audioTrackID)
		}

		fragment, err := mp4.CreateMultiTrackFragment(uint32(i+1), trackIDs)
		if err != nil {
			return nil, fmt.Errorf("创建第 %d 个分片：%w", i, err)
		}

		if i < len(videoFragments) {
			if err := appendFragmentSamples(fragment, videoFragments[i], videoTrex, videoTrackID); err != nil {
				return nil, fmt.Errorf("追加第 %d 个视频分片：%w", i, err)
			}
		}
		if i < len(audioFragments) {
			if err := appendFragmentSamples(fragment, audioFragments[i], audioTrex, audioTrackID); err != nil {
				return nil, fmt.Errorf("追加第 %d 个音频分片：%w", i, err)
			}
		}

		segment := mp4.NewMediaSegmentWithoutStyp()
		segment.AddFragment(fragment)
		segments = append(segments, segment)
	}

	return segments, nil
}

func appendFragmentSamples(dst, src *mp4.Fragment, trex *mp4.TrexBox, trackID uint32) error {
	return appendFragmentSamplesWithOffset(dst, src, trex, trackID, 0)
}

func appendFragmentSamplesWithOffset(dst, src *mp4.Fragment, trex *mp4.TrexBox, trackID uint32, decodeTimeOffset uint64) error {
	fullSamples, err := src.GetFullSamples(trex)
	if err != nil {
		return err
	}
	for _, sample := range fullSamples {
		if sample.DecodeTime >= decodeTimeOffset {
			sample.DecodeTime -= decodeTimeOffset
		} else {
			sample.DecodeTime = 0
		}
		if err := dst.AddFullSampleToTrack(sample, trackID); err != nil {
			return err
		}
	}
	return nil
}

func collectFragments(file *mp4.File) []*mp4.Fragment {
	var fragments []*mp4.Fragment
	for _, segment := range file.Segments {
		fragments = append(fragments, segment.Fragments...)
	}
	return fragments
}
