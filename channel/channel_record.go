package channel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/teacat/chaturbate-dvr/chaturbate"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
)

// Monitor 开始监控频道的直播流并进行录制。
func (ch *Channel) Monitor() {
	client := chaturbate.NewClient()
	ch.Info("开始录制： %s", ch.Config.Username)

	// 创建一个带有取消函数的新上下文，
	// 此 CancelFunc 将存储于频道的 CancelFunc 字段中
	// 并将由 Pause 或 Stop 函数调用
	ctx, _ := ch.WithCancel(context.Background())

	var err error
	for {
		if err = ctx.Err(); err != nil {
			break
		}

		pipeline := func() error {
			return ch.RecordStream(ctx, client)
		}
		onRetry := func(_ uint, err error) {
			ch.UpdateOnlineStatus(false)

			if errors.Is(err, internal.ErrChannelOffline) || errors.Is(err, internal.ErrPrivateStream) {
				ch.Info("频道处于离线或私密状态，%d 分钟后重试", server.Config.Interval)
			} else if errors.Is(err, internal.ErrCloudflareBlocked) {
				ch.Info("频道已被Cloudflare拦截；请尝试使用-cookies和-user-agent参数？%d分钟后重试", server.Config.Interval)
			} else if errors.Is(err, context.Canceled) {
				// ...
			} else {
				ch.Error("重试中：%s：将在 %d 分钟后重试", err.Error(), server.Config.Interval)
			}
		}
		if err = retry.Do(
			pipeline,
			retry.Context(ctx),
			retry.Attempts(0),
			retry.Delay(time.Duration(server.Config.Interval)*time.Minute),
			retry.DelayType(retry.FixedDelay),
			retry.OnRetry(onRetry),
		); err != nil {
			break
		}
	}

	// 监控器退出时始终执行清理操作，无论是否发生错误。
	if err := ch.Cleanup(); err != nil {
		ch.Error("监控器退出时执行清理: %s", err.Error())
	}

	// Log error if it's not a context cancellation
	if err != nil && !errors.Is(err, context.Canceled) {
		ch.Error("录像: %s", err.Error())
	}
}

// Update 向频道的更新通道发送更新信号。
// 此举将通知服务器发送事件，以便向客户端广播频道信息。.
func (ch *Channel) Update() {
	ch.UpdateCh <- true
}

// RecordStream 使用提供的客户端录制频道直播流。
// 该操作会获取流信息并开始监视分片片段。
func (ch *Channel) RecordStream(ctx context.Context, client *chaturbate.Client) error {
	stream, err := client.GetStream(ctx, ch.Config.Username)
	if err != nil {
		return fmt.Errorf("获取流: %w", err)
	}
	ch.StreamedAt = time.Now().Unix()
	ch.Sequence = 0

	if err := ch.NextFile(); err != nil {
		return fmt.Errorf("下一个文件: %w", err)
	}

	// 务必确保函数在任何情况下退出时都会清理文件
	defer func() {
		if err := ch.Cleanup(); err != nil {
			ch.Error("录制流退出时执行清理: %s", err.Error())
		}
	}()

	playlist, err := stream.GetPlaylist(ctx, ch.Config.Resolution, ch.Config.Framerate)
	if err != nil {
		return fmt.Errorf("获取播放列表: %w", err)
	}
	ch.UpdateOnlineStatus(true) // Update online status after `GetPlaylist` is OK

	ch.Info("码流质量 - 分辨率 %dp（目标：%dp），帧率 %dfps（目标：%dfps）", playlist.Resolution, ch.Config.Resolution, playlist.Framerate, ch.Config.Framerate)

	return playlist.WatchSegments(ctx, ch.HandleSegment)
}

// HandleSegment 处理分段数据并将其写入文件。
func (ch *Channel) HandleSegment(b []byte, duration float64) error {
	if ch.Config.IsPaused {
		return retry.Unrecoverable(internal.ErrPaused)
	}

	n, err := ch.File.Write(b)
	if err != nil {
		return fmt.Errorf("写文件: %w", err)
	}

	ch.Filesize += n
	ch.Duration += duration
	ch.Info("时长：%s，文件大小：%s", internal.FormatDuration(ch.Duration), internal.FormatFilesize(ch.Filesize))

	// 发送服务器推送事件（SSE）更新以刷新视图
	ch.Update()

	if ch.ShouldSwitchFile() {
		if err := ch.NextFile(); err != nil {
			return fmt.Errorf("下一个文件: %w", err)
		}
		ch.Info("文件大小或时长超过限制，已创建新文件: %s", ch.File.Name())
		return nil
	}
	return nil
}
