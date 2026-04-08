package channel

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"

	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
)

// Channel represents a channel instance.
type Channel struct {
	CancelFunc context.CancelFunc
	LogCh      chan string
	UpdateCh   chan bool

	IsOnline   bool
	StreamedAt int64
	Duration   float64 // Seconds
	Filesize   int     // Bytes
	Sequence   int

	Logs []string

	File      string
	FfmpegCmd *exec.Cmd
	Config    *entity.ChannelConfig
}

// New creates a new channel instance with the given manager and configuration.
func New(conf *entity.ChannelConfig) *Channel {
	ch := &Channel{
		LogCh:      make(chan string),
		UpdateCh:   make(chan bool),
		Config:     conf,
		CancelFunc: func() {},
	}
	go ch.Publisher()

	return ch
}

// Publisher listens for log messages and updates from the channel
// and publishes once received.
func (ch *Channel) Publisher() {
	for {
		select {
		case v := <-ch.LogCh:
			// Append the log message to ch.Logs and keep only the last 100 rows
			ch.Logs = append(ch.Logs, v)
			if len(ch.Logs) > 100 {
				ch.Logs = ch.Logs[len(ch.Logs)-100:]
			}
			server.Manager.Publish(entity.EventLog, ch.ExportInfo())

		case <-ch.UpdateCh:
			server.Manager.Publish(entity.EventUpdate, ch.ExportInfo())
		}
	}
}

// WithCancel creates a new context with a cancel function,
// then stores the cancel function in the channel's CancelFunc field.
//
// This is used to cancel the context when the channel is stopped or paused.
func (ch *Channel) WithCancel(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, ch.CancelFunc = context.WithCancel(ctx)
	return ctx, ch.CancelFunc
}

// Info logs an informational message.
func (ch *Channel) Info(format string, a ...any) {
	ch.LogCh <- fmt.Sprintf("%s [INFO] %s", time.Now().Format("15:04"), fmt.Sprintf(format, a...))
	log.Printf(" INFO [%s] %s", ch.Config.Username, fmt.Sprintf(format, a...))
}

// Error logs an error message.
func (ch *Channel) Error(format string, a ...any) {
	ch.LogCh <- fmt.Sprintf("%s [ERROR] %s", time.Now().Format("15:04"), fmt.Sprintf(format, a...))
	log.Printf("ERROR [%s] %s", ch.Config.Username, fmt.Sprintf(format, a...))
}

// ExportInfo exports the channel information as a ChannelInfo struct.
func (ch *Channel) ExportInfo() *entity.ChannelInfo {
	var filename string
	if ch.File != "" {
		filename = ch.File
	}
	var streamedAt string
	if ch.StreamedAt != 0 {
		streamedAt = time.Unix(ch.StreamedAt, 0).Format("2006-01-02 15:04 AM")
	}
	return &entity.ChannelInfo{
		IsOnline:     ch.IsOnline,
		IsPaused:     ch.Config.IsPaused,
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
	// 首先取消上下文，停止录制循环
	ch.CancelFunc()

	// 停止 ffmpeg 进程
	if err := ch.StopFfmpeg(); err != nil {
		ch.Error("停止 ffmpeg 失败：%s", err.Error())
	}

	// 清理文件资源，关闭文件句柄
	if err := ch.Cleanup(); err != nil {
		ch.Error("清理文件失败：%s", err.Error())
	}

	ch.Config.IsPaused = true
	ch.Update()
	ch.Info("暂停该频道录制")
}

// Stop stops the channel and cancels the context.
func (ch *Channel) Stop() {
	// Stop the monitoring loop
	ch.CancelFunc()

	// 停止 ffmpeg 进程
	if err := ch.StopFfmpeg(); err != nil {
		ch.Error("停止 ffmpeg 失败：%s", err.Error())
	}

	// 清理文件资源
	if err := ch.Cleanup(); err != nil {
		ch.Error("清理文件失败：%s", err.Error())
	}

	ch.Info("停止该频道录制")
}

// Resume resumes the channel monitoring.
//
// `startSeq` is used to prevent all channels from starting at the same time, preventing TooManyRequests errors.
// It's only be used when program starting and trying to resume all channels at once.
func (ch *Channel) Resume(startSeq int) {
	ch.Config.IsPaused = false

	ch.Update()
	ch.Info("恢复该频道录像")

	<-time.After(time.Duration(startSeq) * time.Second)
	go ch.Monitor()
}

// UpdateOnlineStatus updates the online status of the channel.
func (ch *Channel) UpdateOnlineStatus(isOnline bool) {
	ch.IsOnline = isOnline
	ch.Update()
}
