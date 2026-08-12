package channel

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/teacat/chaturbate-dvr/chaturbate"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
)

// Channel represents a channel instance.
type Channel struct {
	stateMu         sync.RWMutex
	lifecycleMu     sync.Mutex
	monitorRunning  bool
	pauseRunning    bool
	stopped         bool
	monitorDone     chan struct{}
	done            chan struct{}
	publisherDone   chan struct{}
	shutdownOnce    sync.Once
	workerWG        sync.WaitGroup
	compressWG      sync.WaitGroup
	CancelFunc      context.CancelFunc
	PauseCancelFunc context.CancelFunc
	LogCh           chan string
	UpdateCh        chan *entity.ChannelInfo

	IsOnline   bool
	RoomStatus string // public, private, group, away, offline
	StreamedAt int64
	Duration   float64 // Seconds
	Filesize   int     // Bytes
	Sequence   int

	Logs []string

	File               *os.File
	AudioFile          *os.File
	Config             *entity.ChannelConfig
	CurrentFilename    string
	InitSegment        []byte // fMP4 video init segment for LL-HLS streams
	AudioInitSegment   []byte // fMP4 audio init segment for LL-HLS streams
	HasSeparateAudio   bool
	RealtimeMux        bool
	CombinedInit       []byte
	MuxSequence        uint32
	RealtimeMuxer      *realtimeMuxer
	LastFileSync       time.Time
	FragmentsSinceSync int
	LastProgressUpdate time.Time
	switchRequested    bool // set by HandleSegment, consumed by OnPollComplete
	videoMediaBytes    int
	audioMediaBytes    int
}

// New creates a new channel instance with the given manager and configuration.
func New(conf *entity.ChannelConfig) *Channel {
	ch := &Channel{
		LogCh:           make(chan string, 100),
		UpdateCh:        make(chan *entity.ChannelInfo, 1),
		Config:          conf,
		CancelFunc:      func() {},
		PauseCancelFunc: func() {},
		done:            make(chan struct{}),
		publisherDone:   make(chan struct{}),
	}
	go ch.Publisher()

	return ch
}

// Publisher listens for log messages and updates from the channel
// and publishes once received.
func (ch *Channel) Publisher() {
	defer close(ch.publisherDone)
	for {
		select {
		case <-ch.done:
			return
		case v := <-ch.LogCh:
			// Append the log message to ch.Logs and keep only the last 100 rows
			ch.stateMu.Lock()
			ch.Logs = append(ch.Logs, v)
			if len(ch.Logs) > 100 {
				ch.Logs = ch.Logs[len(ch.Logs)-100:]
			}
			info := &entity.ChannelInfo{Username: ch.Config.Username, Logs: append([]string(nil), ch.Logs...)}
			ch.stateMu.Unlock()
			server.Manager.Publish(entity.EventLog, info)

		case info := <-ch.UpdateCh:
			server.Manager.Publish(entity.EventUpdate, info)
		}
	}
}

// WithCancel creates a new context with a cancel function,
// then stores the cancel function in the channel's CancelFunc field.
//
// This is used to cancel the context when the channel is stopped or paused.
func (ch *Channel) WithCancel(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	ch.lifecycleMu.Lock()
	if ch.stopped {
		cancel()
	} else {
		ch.CancelFunc = cancel
	}
	ch.lifecycleMu.Unlock()
	return ctx, cancel
}

// Info logs an informational message.
func (ch *Channel) Info(format string, a ...any) {
	message := fmt.Sprintf(format, a...)
	ch.enqueueLog(fmt.Sprintf("%s [信息] %s", time.Now().Format("15:04"), message))
	log.Printf("[信息] [%s] %s", ch.Config.Username, message)
}

// Error logs an error message.
func (ch *Channel) Error(format string, a ...any) {
	message := fmt.Sprintf(format, a...)
	ch.enqueueLog(fmt.Sprintf("%s [错误] %s", time.Now().Format("15:04"), message))
	log.Printf("[错误] [%s] %s", ch.Config.Username, message)
}

func roomStatusText(status string) string {
	switch status {
	case chaturbate.StatusPublic:
		return "公开直播"
	case chaturbate.StatusPrivate:
		return "私密直播"
	case chaturbate.StatusAway:
		return "暂时离开"
	case chaturbate.StatusOffline:
		return "离线"
	case "group":
		return "群组直播"
	case "hidden":
		return "隐藏"
	case "password protected":
		return "密码保护"
	case "":
		return "未知"
	default:
		return status
	}
}

func (ch *Channel) enqueueLog(message string) {
	select {
	case ch.LogCh <- message:
	default:
		select {
		case <-ch.LogCh:
		default:
		}
		select {
		case ch.LogCh <- message:
		default:
		}
	}
}

// ExportInfo exports the channel information as a ChannelInfo struct.
func (ch *Channel) ExportInfo() *entity.ChannelInfo {
	ch.stateMu.RLock()
	defer ch.stateMu.RUnlock()
	return ch.exportInfoLocked()
}

func (ch *Channel) ConfigSnapshot() *entity.ChannelConfig {
	ch.stateMu.RLock()
	defer ch.stateMu.RUnlock()
	copy := *ch.Config
	return &copy
}

// ApplyGlobalRecordingSettings snapshots the global recording policy at the
// start of a session. Active files are never reconfigured halfway through.
func (ch *Channel) ApplyGlobalRecordingSettings() {
	if server.Config == nil {
		return
	}
	server.ConfigMu.RLock()
	framerate, resolution, pattern := server.Config.Framerate, server.Config.Resolution, server.Config.Pattern
	maxDuration, maxFilesize, compress := server.Config.MaxDuration, server.Config.MaxFilesize, server.Config.Compress
	server.ConfigMu.RUnlock()
	if framerate <= 0 || resolution <= 0 || pattern == "" {
		return
	}
	ch.stateMu.Lock()
	ch.Config.Framerate, ch.Config.Resolution, ch.Config.Pattern = framerate, resolution, pattern
	ch.Config.MaxDuration, ch.Config.MaxFilesize, ch.Config.Compress = maxDuration, maxFilesize, compress
	ch.stateMu.Unlock()
}

func (ch *Channel) exportInfoLocked() *entity.ChannelInfo {
	var filename string
	if ch.CurrentFilename != "" && ch.RealtimeMux {
		filename = realtimeWorkingPath(ch.CurrentFilename)
	} else if ch.CurrentFilename != "" && ch.HasSeparateAudio {
		filename = ch.CurrentFilename + ".mp4"
	} else if ch.File != nil {
		filename = ch.File.Name()
	}
	var streamedAt string
	if ch.StreamedAt != 0 {
		streamedAt = time.Unix(ch.StreamedAt, 0).Format("2006-01-02 15:04 AM")
	}
	return &entity.ChannelInfo{
		IsOnline:     ch.IsOnline,
		IsPaused:     ch.Config.IsPaused,
		RoomStatus:   ch.RoomStatus,
		Username:     ch.Config.Username,
		MaxDuration:  internal.FormatDuration(float64(ch.Config.MaxDuration * 60)), // MaxDuration from config is in minutes
		MaxFilesize:  internal.FormatFilesize(ch.Config.MaxFilesize * 1024 * 1024), // MaxFilesize from config is in MB
		StreamedAt:   streamedAt,
		CreatedAt:    ch.Config.CreatedAt,
		Duration:     internal.FormatDuration(ch.Duration),
		Filesize:     internal.FormatFilesize(ch.Filesize),
		Filename:     filename,
		Logs:         ch.Logs,
		GlobalConfig: server.Config,
	}
}

// Pause pauses the channel and cancels the context.
func (ch *Channel) Pause() {
	ch.stateMu.RLock()
	alreadyPaused := ch.Config.IsPaused
	ch.stateMu.RUnlock()
	ch.lifecycleMu.Lock()
	if ch.stopped || alreadyPaused {
		ch.lifecycleMu.Unlock()
		return
	}
	cancel := ch.CancelFunc
	ch.lifecycleMu.Unlock()

	// Stop the monitoring loop, this also updates `ch.IsOnline` to false
	// `context.Canceled` → `ch.Monitor()` → `onRetry` → `ch.UpdateOnlineStatus(false)`.
	cancel()

	ch.stateMu.Lock()
	ch.Config.IsPaused = true
	ch.stateMu.Unlock()
	ch.Update()
	ch.Info("频道已暂停")

	ch.StartPausedWatcher(0)
}

func (ch *Channel) StartPausedWatcher(startSeq int) {
	ch.lifecycleMu.Lock()
	if ch.stopped || ch.pauseRunning {
		ch.lifecycleMu.Unlock()
		return
	}
	ch.pauseRunning = true
	ctx, pauseCancel := context.WithCancel(context.Background())
	ch.PauseCancelFunc = pauseCancel
	ch.workerWG.Add(1)
	ch.lifecycleMu.Unlock()
	go func() {
		defer ch.workerWG.Done()
		ch.CheckOnlineWhilePaused(ctx, startSeq)
		ch.lifecycleMu.Lock()
		ch.pauseRunning = false
		ch.lifecycleMu.Unlock()
	}()
}

// Stop stops the channel and cancels the context.
func (ch *Channel) Stop() {
	ch.shutdownOnce.Do(func() {
		ch.lifecycleMu.Lock()
		ch.stopped = true
		cancel, pauseCancel := ch.CancelFunc, ch.PauseCancelFunc
		ch.lifecycleMu.Unlock()
		cancel()
		pauseCancel()
		ch.Info("频道已停止")
		close(ch.done)
		ch.workerWG.Wait()
		ch.compressWG.Wait()
		<-ch.publisherDone
	})
}

// Resume resumes the channel monitoring.
//
// `startSeq` is used to prevent all channels from starting at the same time, preventing TooManyRequests errors.
// It's only be used when program starting and trying to resume all channels at once.
func (ch *Channel) Resume(startSeq int) {
	ch.stateMu.RLock()
	wasPaused := ch.Config.IsPaused
	ch.stateMu.RUnlock()
	ch.lifecycleMu.Lock()
	if ch.stopped {
		ch.lifecycleMu.Unlock()
		return
	}
	pauseCancel := ch.PauseCancelFunc
	if ch.monitorRunning {
		if !wasPaused {
			ch.lifecycleMu.Unlock()
			return
		}
		done := ch.monitorDone
		ch.stateMu.Lock()
		ch.Config.IsPaused = false
		ch.stateMu.Unlock()
		ch.lifecycleMu.Unlock()
		pauseCancel()
		go func() {
			<-done
			ch.Resume(startSeq)
		}()
		return
	}
	ch.monitorRunning = true
	ch.monitorDone = make(chan struct{})
	done := ch.monitorDone
	ch.workerWG.Add(1)
	ch.lifecycleMu.Unlock()
	pauseCancel()
	ch.stateMu.Lock()
	ch.Config.IsPaused = false
	ch.stateMu.Unlock()

	ch.Update()
	ch.Info("频道已恢复")

	go func() {
		defer ch.workerWG.Done()
		if startSeq > 0 {
			timer := time.NewTimer(time.Duration(startSeq) * time.Second)
			select {
			case <-ch.done:
				timer.Stop()
				ch.finishMonitor(done)
				return
			case <-timer.C:
			}
		}
		select {
		case <-ch.done:
			ch.finishMonitor(done)
			return
		default:
		}
		ch.Monitor()
		ch.finishMonitor(done)
	}()
}

func (ch *Channel) finishMonitor(done chan struct{}) {
	ch.lifecycleMu.Lock()
	if ch.monitorDone == done {
		ch.monitorRunning = false
		close(done)
	}
	ch.lifecycleMu.Unlock()
}

// UpdateOnlineStatus updates the online status of the channel.
func (ch *Channel) UpdateOnlineStatus(isOnline bool) {
	ch.stateMu.Lock()
	ch.IsOnline = isOnline
	ch.stateMu.Unlock()
	ch.Update()
}

// CheckOnlineWhilePaused periodically refreshes room status for paused channels
// so the UI can still distinguish online/private/offline states.
func (ch *Channel) CheckOnlineWhilePaused(ctx context.Context, startSeq int) {
	client := chaturbate.NewClient()
	baseIntervalMinutes := max(server.Config.Interval, 15)
	cfBlockCount := 0

	initialDelay := time.Duration(startSeq*5) * time.Second
	if initialDelay > 0 {
		timer := time.NewTimer(initialDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}

	for {
		waitInterval := time.Duration(baseIntervalMinutes) * time.Minute

		status, err := client.GetRoomStatus(ctx, ch.Config.Username)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			if isCFBlock(err) {
				cfBlockCount++
				delayMinutes := cfBackoffMinutes(cfBlockCount, baseIntervalMinutes)
				waitInterval = time.Duration(delayMinutes) * time.Minute
				ch.Info("暂停期间的状态检查被 Cloudflare 拦截（第 %d 次）；%d 分钟后重试", cfBlockCount, delayMinutes)
			} else {
				cfBlockCount = 0
			}
		} else if status != "" {
			cfBlockCount = 0
			isOnline := status != chaturbate.StatusAway && status != chaturbate.StatusOffline
			ch.stateMu.Lock()
			changed := ch.IsOnline != isOnline || ch.RoomStatus != status
			if changed {
				ch.IsOnline = isOnline
				ch.RoomStatus = status
			}
			ch.stateMu.Unlock()
			if changed {
				ch.Info("频道状态：%s（当前已暂停）", roomStatusText(status))
				ch.Update()
			}
		}

		timer := time.NewTimer(waitInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}
