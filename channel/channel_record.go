package channel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/teacat/chaturbate-dvr/chaturbate"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
)

// Monitor starts monitoring the channel for live streams and records them.
func (ch *Channel) Monitor() {
	client := chaturbate.NewClient()
	ch.Info("开始监控频道 `%s`", ch.Config.Username)

	// Create a new context with a cancel function,
	// the CancelFunc will be stored in the channel's CancelFunc field
	// and will be called by `Pause` or `Stop` functions
	ctx, _ := ch.WithCancel(context.Background())

	var err error
	for {
		if err = ctx.Err(); err != nil {
			break
		}

		pipeline := func() error {
			return ch.RecordStream(ctx, client)
		}

		cfBlockCount := 0

		onRetry := func(_ uint, err error) {
			if errors.Is(err, internal.ErrPlaylistForbidden) || errors.Is(err, internal.ErrTimelineReset) {
				ch.Info("直播会话或时间轴已变化：%s；立即轮转文件并刷新直播地址", err.Error())
				return
			}
			ch.UpdateOnlineStatus(false)

			if isCFBlock(err) {
				cfBlockCount++
				delay := cfBackoffMinutes(cfBlockCount, server.Config.Interval)
				ch.Info("被 Cloudflare 拦截（第 %d 次）；请检查 Cookies 和 User-Agent，%d 分钟后重试", cfBlockCount, delay)
			} else if errors.Is(err, internal.ErrChannelOffline) || errors.Is(err, internal.ErrPrivateStream) {
				cfBlockCount = 0
				ch.RoomStatus = client.LastRoomStatus
				ch.Update()
				ch.Info("频道状态：%s；%d 分钟后重试", roomStatusText(ch.RoomStatus), server.Config.Interval)
			} else if errors.Is(err, context.Canceled) {
				cfBlockCount = 0
			} else {
				cfBlockCount = 0
				ch.Error("录制失败：%s；%d 分钟后重试", err.Error(), server.Config.Interval)
			}
		}

		customDelay := func(attempt uint, err error, _ *retry.Config) time.Duration {
			if errors.Is(err, internal.ErrPlaylistForbidden) || errors.Is(err, internal.ErrTimelineReset) {
				return 0
			}
			if isCFBlock(err) {
				return time.Duration(cfBackoffMinutes(cfBlockCount, server.Config.Interval)) * time.Minute
			}
			if delay := internal.HTTPRetryAfter(err); delay > 0 {
				return delay
			}
			if status := internal.HTTPStatusCode(err); status >= 500 && status <= 599 {
				base := time.Duration(max(server.Config.Interval, 1)) * time.Minute
				return base * time.Duration(1<<min(attempt, 4))
			}
			return time.Duration(server.Config.Interval) * time.Minute
		}

		if err = retry.Do(
			pipeline,
			retry.Context(ctx),
			retry.Attempts(0),
			retry.DelayType(customDelay),
			retry.OnRetry(onRetry),
		); err != nil {
			break
		}
	}

	// Always cleanup when monitor exits, regardless of error
	if err := ch.Cleanup(); err != nil {
		ch.Error("监控退出时清理失败：%s", err.Error())
	}

	// Log error if it's not a context cancellation
	if err != nil && !errors.Is(err, context.Canceled) {
		ch.Error("录制流失败：%s", err.Error())
	}
}

// Update sends an update signal to the channel's update channel.
// This notifies the Server-sent Event to boradcast the channel information to the client.
func (ch *Channel) Update() {
	info := ch.ExportInfo()
	select {
	case ch.UpdateCh <- info:
	default:
		select {
		case <-ch.UpdateCh:
		default:
		}
		select {
		case ch.UpdateCh <- info:
		default:
		}
	}
}

// RecordStream records the stream of the channel using the provided client.
// It retrieves the stream information and starts watching the segments.
func (ch *Channel) RecordStream(ctx context.Context, client *chaturbate.Client) error {
	ch.ApplyGlobalRecordingSettings()
	stream, err := client.GetStream(ctx, ch.Config.Username)
	if err != nil {
		return fmt.Errorf("获取直播地址：%w", err)
	}
	playlist, err := stream.GetPlaylist(ctx, ch.Config.Resolution, ch.Config.Framerate)
	if err != nil {
		return fmt.Errorf("获取播放列表：%w", err)
	}

	ch.StreamedAt = time.Now().Unix()
	ch.Sequence = 0
	ch.InitSegment = nil
	ch.AudioInitSegment = nil
	ch.HasSeparateAudio = playlist.AudioPlaylistURL != ""
	ch.RealtimeMux = false
	ch.CombinedInit = nil
	ch.MuxSequence = 0
	ch.RealtimeMuxer = nil
	ch.FragmentsSinceSync = 0
	ch.switchRequested = false

	if err := ch.NextFile(); err != nil {
		return fmt.Errorf("创建录像文件：%w", err)
	}

	// Ensure file is cleaned up when this function exits in any case
	defer func() {
		if err := ch.Cleanup(); err != nil {
			ch.Error("录制退出时清理失败：%s", err.Error())
		}
	}()

	ch.RoomStatus = chaturbate.StatusPublic
	ch.UpdateOnlineStatus(true) // after GetPlaylist succeeds

	ch.Info("直播质量：分辨率 %dp（目标 %dp），帧率 %d FPS（目标 %d FPS）", playlist.Resolution, ch.Config.Resolution, playlist.Framerate, ch.Config.Framerate)
	if ch.HasSeparateAudio {
		ch.Info("检测到独立音频轨，将同步录制并合并音视频")
	}

	return playlist.WatchAVSegments(ctx, ch.HandleSegment, ch.HandleInitSegment, ch.HandleAudioSegment, ch.HandleAudioInitSegment, ch.HandleAVInitSegments, ch.HandleAVSegments, ch.OnPollComplete)
}

// HandleInitSegment stores the fMP4 init segment and reopens the file with the correct extension.
func (ch *Channel) HandleInitSegment(initData []byte) error {
	ch.InitSegment = initData

	if ch.File == nil {
		return nil
	}

	oldName := ch.File.Name()
	if err := ch.File.Close(); err != nil {
		return fmt.Errorf("重命名前关闭视频文件：%w", err)
	}
	ch.File = nil

	newName := strings.TrimSuffix(oldName, filepath.Ext(oldName)) + ".mp4"
	if err := moveFile(oldName, newName); err != nil {
		return fmt.Errorf("将视频文件重命名为 MP4：%w", err)
	}

	file, err := os.OpenFile(newName, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		_ = os.Remove(newName)
		return fmt.Errorf("重新打开 MP4 视频文件：%w", err)
	}
	ch.File = file

	n, err := ch.File.Write(initData)
	if err != nil {
		_ = ch.File.Close()
		ch.File = nil
		_ = os.Remove(newName)
		return fmt.Errorf("写入视频初始化分片：%w", err)
	}
	ch.Filesize += n
	return nil
}

// HandleAudioInitSegment stores the fMP4 audio init segment and reopens the audio file with the correct extension.
func (ch *Channel) HandleAudioInitSegment(initData []byte) error {
	ch.AudioInitSegment = initData

	if ch.AudioFile == nil {
		return nil
	}

	oldName := ch.AudioFile.Name()
	if err := ch.AudioFile.Close(); err != nil {
		return fmt.Errorf("重命名前关闭音频文件：%w", err)
	}
	ch.AudioFile = nil

	newName := strings.TrimSuffix(oldName, filepath.Ext(oldName)) + ".mp4"
	if err := moveFile(oldName, newName); err != nil {
		return fmt.Errorf("将音频文件重命名为 MP4：%w", err)
	}

	file, err := os.OpenFile(newName, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		_ = os.Remove(newName)
		return fmt.Errorf("重新打开 MP4 音频文件：%w", err)
	}
	ch.AudioFile = file

	if _, err := ch.AudioFile.Write(initData); err != nil {
		_ = ch.AudioFile.Close()
		ch.AudioFile = nil
		_ = os.Remove(newName)
		return fmt.Errorf("写入音频初始化分片：%w", err)
	}
	return nil
}

// HandleAVInitSegments replaces the temporary track files with one playable
// fragmented MP4 containing both track descriptions.
func (ch *Channel) HandleAVInitSegments(videoInit, audioInit []byte) error {
	muxer, err := newRealtimeMuxer(videoInit, audioInit)
	if err != nil {
		return err
	}
	combined := muxer.initData
	ch.InitSegment = videoInit
	ch.AudioInitSegment = audioInit
	ch.CombinedInit = combined
	ch.RealtimeMux = true
	ch.MuxSequence = 0
	ch.RealtimeMuxer = muxer
	ch.FragmentsSinceSync = 0
	ch.LastFileSync = time.Now()

	outputPath := realtimeWorkingPath(ch.CurrentFilename)
	file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("创建实时 MP4：%w", err)
	}
	n, err := file.Write(combined)
	if err != nil {
		_ = file.Close()
		_ = os.Remove(outputPath)
		return fmt.Errorf("写入双轨初始化信息：%w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(outputPath)
		return fmt.Errorf("同步双轨初始化信息：%w", err)
	}
	for _, oldFile := range []*os.File{ch.File, ch.AudioFile} {
		if oldFile != nil {
			name := oldFile.Name()
			_ = oldFile.Close()
			_ = os.Remove(name)
		}
	}
	ch.File, ch.AudioFile = file, nil
	ch.Filesize = n
	return nil
}

// HandleAVSegments appends one timeline-aligned, two-track fMP4 fragment.
func (ch *Channel) HandleAVSegments(video, audio []chaturbate.TimedSegment) error {
	if ch.Config.IsPaused {
		return retry.Unrecoverable(internal.ErrPaused)
	}
	ch.MuxSequence++
	if ch.RealtimeMuxer == nil {
		return fmt.Errorf("实时音视频合并器尚未初始化")
	}
	videoData := make([][]byte, len(video))
	for i := range video {
		videoData[i] = video[i].Data
	}
	audioData := make([][]byte, len(audio))
	for i := range audio {
		audioData[i] = audio[i].Data
	}
	data, err := ch.RealtimeMuxer.combineMediaGroup(videoData, audioData, ch.MuxSequence)
	if err != nil {
		return fmt.Errorf("实时合并音视频分片：%w", err)
	}
	if err := ch.ensureDiskSpace(len(data)); err != nil {
		return retry.Unrecoverable(err)
	}
	n, err := ch.File.Write(data)
	if err != nil {
		return fmt.Errorf("写入实时 MP4：%w", err)
	}
	ch.FragmentsSinceSync++
	if err := ch.syncRealtimeFile(false); err != nil {
		return err
	}
	for _, segment := range video {
		ch.videoMediaBytes += len(segment.Data)
	}
	for _, segment := range audio {
		ch.audioMediaBytes += len(segment.Data)
	}
	ch.Filesize += n
	ch.Duration += video[len(video)-1].End - video[0].Start
	ch.reportProgress()
	if ch.ShouldSwitchFile() {
		ch.switchRequested = true
	}
	return nil
}

const (
	realtimeSyncInterval  = 3 * time.Second
	realtimeSyncFragments = 10
	minimumFreeDiskBytes  = 512 << 20
)

func (ch *Channel) ensureDiskSpace(additionalBytes int) error {
	if ch.File == nil {
		return nil
	}
	free, err := availableDiskBytes(filepath.Dir(ch.File.Name()))
	if err != nil {
		return fmt.Errorf("检查磁盘剩余空间：%w", err)
	}
	minimumFree := configuredMinimumFreeDiskBytes()
	if free < uint64(minimumFree+additionalBytes) {
		return fmt.Errorf("%w：仅剩 %s", internal.ErrDiskSpace, internal.FormatFilesize(int(free)))
	}
	return nil
}

func configuredMinimumFreeDiskBytes() int {
	minimumFree := minimumFreeDiskBytes
	server.ConfigMu.RLock()
	if server.Config != nil && server.Config.MinFreeDiskMB >= 0 {
		minimumFree = server.Config.MinFreeDiskMB << 20
	}
	server.ConfigMu.RUnlock()
	return minimumFree
}

func (ch *Channel) syncRealtimeFile(force bool) error {
	if ch.File == nil {
		return nil
	}
	fragmentLimit := realtimeSyncFragments
	interval := realtimeSyncInterval
	if server.Config != nil {
		if server.Config.SyncFragments > 0 {
			fragmentLimit = server.Config.SyncFragments
		}
		if server.Config.SyncSeconds > 0 {
			interval = time.Duration(server.Config.SyncSeconds) * time.Second
		}
	}
	if !force && ch.FragmentsSinceSync < fragmentLimit && time.Since(ch.LastFileSync) < interval {
		return nil
	}
	if err := ch.File.Sync(); err != nil {
		return fmt.Errorf("同步实时 MP4：%w", err)
	}
	ch.LastFileSync = time.Now()
	ch.FragmentsSinceSync = 0
	return nil
}

func isCFBlock(err error) bool {
	return errors.Is(err, internal.ErrCloudflareBlocked) || errors.Is(err, internal.ErrAgeVerification)
}

// cfBackoffMinutes returns the delay in minutes for Cloudflare block retries.
// Uses exponential backoff: interval * 2^(n-1), capped at 30 minutes.
// consecutiveBlocks must be >= 1.
func cfBackoffMinutes(consecutiveBlocks, baseInterval int) int {
	shift := min(consecutiveBlocks-1, 4) // max multiplier: 16x
	delay := baseInterval * (1 << shift)
	return min(delay, 30)
}

// HandleSegment processes and writes segment data to a file.
func (ch *Channel) HandleSegment(b []byte, duration float64) error {
	if ch.Config.IsPaused {
		return retry.Unrecoverable(internal.ErrPaused)
	}
	if err := ch.ensureDiskSpace(len(b)); err != nil {
		return retry.Unrecoverable(err)
	}

	n, err := ch.File.Write(b)
	ch.videoMediaBytes += n
	if err != nil {
		return fmt.Errorf("写入视频文件：%w", err)
	}

	ch.Filesize += n
	ch.Duration += duration
	ch.reportProgress()

	if !ch.ShouldSwitchFile() {
		return nil
	}

	// For LL-HLS streams with separate audio, defer the rotation until the
	// current poll cycle finishes so the paired audio segments land in the
	// same file as the video ones. Single-stream recordings have no pairing
	// risk, and deferring would let processMediaPlaylist keep appending a
	// backlog of catch-up segments past the MaxFilesize/MaxDuration limit.
	if ch.HasSeparateAudio {
		ch.switchRequested = true
		return nil
	}

	if err := ch.NextFile(); err != nil {
		return fmt.Errorf("轮转录像文件：%w", err)
	}
	ch.Info("达到文件大小或时长上限，已创建新文件：%s", ch.File.Name())
	return nil
}

func (ch *Channel) reportProgress() {
	if time.Since(ch.LastProgressUpdate) < time.Second {
		return
	}
	ch.LastProgressUpdate = time.Now()
	ch.Info("已录制：%s，文件大小：%s", internal.FormatDuration(ch.Duration), internal.FormatFilesize(ch.Filesize))
	ch.Update()
}

// OnPollComplete performs any file rotation requested during the poll cycle.
// Called by WatchAVSegments after both video and audio playlists have been
// processed, guaranteeing that rotation never splits an A/V pair.
func (ch *Channel) OnPollComplete() error {
	if !ch.switchRequested {
		return nil
	}
	ch.switchRequested = false
	if err := ch.NextFile(); err != nil {
		return fmt.Errorf("轮转录像文件：%w", err)
	}
	ch.Info("达到文件大小或时长上限，已创建新文件：%s", ch.File.Name())
	return nil
}

// HandleAudioSegment processes and writes audio segment data to a sidecar file.
func (ch *Channel) HandleAudioSegment(b []byte, _ float64) error {
	if ch.AudioFile == nil {
		return nil
	}
	if ch.Config.IsPaused {
		return retry.Unrecoverable(internal.ErrPaused)
	}
	if err := ch.ensureDiskSpace(len(b)); err != nil {
		return retry.Unrecoverable(err)
	}

	n, err := ch.AudioFile.Write(b)
	ch.audioMediaBytes += n
	if err != nil {
		return fmt.Errorf("写入音频文件：%w", err)
	}
	return nil
}
