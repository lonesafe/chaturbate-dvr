package channel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/teacat/chaturbate-dvr/chaturbate"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// Monitor 监控频道状态并启动录制
func (ch *Channel) Monitor() {
	client := chaturbate.NewClient()
	ch.Info("开始录制：%s", ch.Config.Username)

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
				ch.Info("频道已被 Cloudflare 拦截；请尝试使用-cookies 和-user-agent 参数？%d 分钟后重试", server.Config.Interval)
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

	if err := ch.Cleanup(); err != nil {
		ch.Error("监控器退出时执行清理：%s", err.Error())
	}

	if err != nil && !errors.Is(err, context.Canceled) {
		ch.Error("录像：%s", err.Error())
	}
}

// Update 发送更新信号到更新通道
func (ch *Channel) Update() {
	ch.UpdateCh <- true
}

// RecordStream 录制直播流
func (ch *Channel) RecordStream(ctx context.Context, client *chaturbate.Client) error {
	// 获取直播流信息
	stream, err := client.GetStream(ctx, ch.Config.Username)
	if err != nil {
		return fmt.Errorf("获取流：%w", err)
	}

	// 记录开始时间
	ch.StreamedAt = time.Now().Unix()
	ch.Sequence = 0

	// 创建新文件
	if err := ch.NextFile(); err != nil {
		return fmt.Errorf("下一个文件：%w", err)
	}

	// 获取播放列表（包含 HLS URL）
	playlist, err := stream.GetPlaylist(ctx, ch.Config.Resolution, ch.Config.Framerate)
	if err != nil {
		return fmt.Errorf("获取播放列表：%w", err)
	}
	ch.UpdateOnlineStatus(true)

	ch.Info("码流质量 - 分辨率 %dp（目标：%dp），帧率 %dfps（目标：%dfps）", playlist.Resolution, ch.Config.Resolution, playlist.Framerate, ch.Config.Framerate)

	// 使用 ffmpeg 开始录制
	return ch.StartFfmpegRecording(ctx, playlist.PlaylistURL)
}

// StartFfmpegRecording 启动 ffmpeg 录制 HLS 流
func (ch *Channel) StartFfmpegRecording(ctx context.Context, hlsURL string) error {
	filename := ch.File

	// 构建 ffmpeg 命令参数
	args := []string{
		"-y",              // 覆盖输出文件
		"-reconnect", "1", // 启用断线重连
		"-reconnect_at_eof", "1", // 在 EOF 时重连
		//"-reconnect_streamed", "1", // 对流媒体启用重连
		"-i", hlsURL, // 输入 HLS URL
		"-c:v", "copy", // 视频直接复制
		"-c:a", "copy", // 音频直接复制
		"-movflags", "+faststart+frag_keyframe+empty_moov+default_base_moof", // 优化 MP4 结构
		"-fflags", "+genpts+igndts", // 生成 PTS，忽略 DTS
		"-max_muxing_queue_size", "1024", // 增加复用队列大小
		"-avoid_negative_ts", "make_zero", // 避免负时间戳
	}

	//// 如果配置了 User-Agent，添加到命令中
	//if server.Config.UserAgent != "" {
	//	args = append(args, "-user_agent", server.Config.UserAgent)
	//}
	//
	//// 如果配置了 Cookies，添加到命令中
	//if server.Config.Cookies != "" {
	//	args = append(args, "-cookies", server.Config.Cookies)
	//}

	// 添加输出文件名
	args = append(args, filename)

	ch.Info("启动 ffmpeg: %s", strings.Join(args, " "))

	// 创建 ffmpeg 命令
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	// 获取标准错误输出管道（用于监控进度）
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("创建 stderr 管道失败：%w", err)
	}

	// 启动 ffmpeg 进程
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 ffmpeg 失败：%w", err)
	}

	ch.FfmpegCmd = cmd

	// 在后台监控 ffmpeg 输出
	go ch.monitorFfmpegProgress(stderr)

	// 等待 ffmpeg 完成
	if err := cmd.Wait(); err != nil {
		if ctx.Err() == context.Canceled {
			return nil
		}
		return fmt.Errorf("ffmpeg 执行失败：%w", err)
	}

	return nil
}

// monitorFfmpegProgress 监控 ffmpeg 的输出以跟踪录制进度
func (ch *Channel) monitorFfmpegProgress(stderr io.ReadCloser) {
	defer stderr.Close()

	buf := make([]byte, 4096)
	for {
		n, err := stderr.Read(buf)
		if err != nil {
			break
		}

		output := string(buf[:n])

		// 检查是否包含时间信息
		if strings.Contains(output, "time=") {
			timeIdx := strings.Index(output, "time=")
			if timeIdx != -1 {
				timeStr := output[timeIdx+5:]
				endIdx := strings.Index(timeStr, " ")
				if endIdx != -1 {
					timeStr = timeStr[:endIdx]
					// 解析时长
					duration := ch.parseDuration(timeStr)
					ch.Duration = duration

					// 获取文件大小
					fileInfo, err := os.Stat(ch.File)
					if err == nil {
						ch.Filesize = int(fileInfo.Size())
					}

					// 记录进度
					ch.Info("录制进度 - 时长：%s，文件大小：%s",
						internal.FormatDuration(ch.Duration),
						internal.FormatFilesize(ch.Filesize))

					ch.Update()

					// 检查是否需要切换文件
					if ch.ShouldSwitchFile() {
						ch.Info("文件大小或时长超过限制，准备切换文件")
						if err := ch.switchFileWithFfmpeg(); err != nil {
							ch.Error("切换文件失败：%s", err.Error())
						}
					}
				}
			}
		}
	}
}

// parseDuration 解析 ffmpeg 的时间字符串（格式：HH:MM:SS.mmm）
func (ch *Channel) parseDuration(timeStr string) float64 {
	parts := strings.Split(timeStr, ":")
	if len(parts) != 3 {
		return 0
	}

	hours, _ := strconv.ParseFloat(parts[0], 64)
	minutes, _ := strconv.ParseFloat(parts[1], 64)
	seconds, _ := strconv.ParseFloat(parts[2], 64)

	return hours*3600 + minutes*60 + seconds
}

// switchFileWithFfmpeg 切换到一个新的录制文件
func (ch *Channel) switchFileWithFfmpeg() error {
	// 停止当前的 ffmpeg 进程
	if err := ch.StopFfmpeg(); err != nil {
		return fmt.Errorf("停止 ffmpeg 失败：%w", err)
	}

	// 创建新文件
	if err := ch.NextFile(); err != nil {
		return fmt.Errorf("创建新文件失败：%w", err)
	}

	ch.Info("已切换到新文件：%s", ch.File)
	return nil
}

// StopFfmpeg 停止 ffmpeg 进程
func (ch *Channel) StopFfmpeg() error {
	if ch.FfmpegCmd == nil {
		return nil
	}

	if ch.FfmpegCmd.Process != nil {
		ch.Info("正在停止 ffmpeg 进程...")

		var err error

		// 根据操作系统选择不同的终止方式
		if runtime.GOOS == "windows" {
			// Windows: 使用 taskkill 命令
			err = ch.terminateProcessOnWindows()
		} else {
			// Unix-like: 使用信号
			err = ch.terminateProcessOnUnix()
		}

		if err != nil {
			return fmt.Errorf("停止 ffmpeg 失败：%w", err)
		}
	}

	ch.FfmpegCmd = nil
	return nil
}

// terminateProcessOnWindows 在 Windows 上终止进程
func (ch *Channel) terminateProcessOnWindows() error {
	pid := ch.FfmpegCmd.Process.Pid
	ch.FfmpegCmd.Process.Kill()

	// 首先尝试使用 taskkill /T 终止进程树（包括子进程）
	cmd := exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprintf("%d", pid))
	output, err := cmd.CombinedOutput()
	outputStr := decodeGBK(output)
	if err != nil {
		// 如果进程已经不存在，这不算错误
		// 检查常见的"进程不存在"错误信息（支持中英文）
		if strings.Contains(outputStr, "not found") ||
			strings.Contains(outputStr, "没有找到") ||
			strings.Contains(outputStr, "找不到") ||
			strings.Contains(outputStr, "no running instance") ||
			strings.Contains(outputStr, "没有运行的实例") {
			ch.Info("ffmpeg 进程已不存在 (PID: %d)", pid)
			return nil
		}
		return fmt.Errorf("taskkill 失败：%s：%w", outputStr, err)
	}

	ch.Info("已通过 taskkill 终止 ffmpeg 进程 (PID: %d)", pid)
	return nil
}

// terminateProcessOnUnix 在 Unix-like 系统上终止进程
func (ch *Channel) terminateProcessOnUnix() error {
	// 先尝试发送 SIGINT 信号（优雅中断）
	if err := ch.FfmpegCmd.Process.Signal(syscall.SIGINT); err != nil {
		ch.Error("发送 SIGINT 信号失败：%s，尝试强制终止", err.Error())

		// 如果 SIGINT 失败，尝试 SIGTERM
		if err := ch.FfmpegCmd.Process.Signal(syscall.SIGTERM); err != nil {
			ch.Error("发送 SIGTERM 信号失败：%s，使用 Kill", err.Error())

			// 最后手段：强制终止
			if err := ch.FfmpegCmd.Process.Kill(); err != nil {
				return fmt.Errorf("强制终止失败：%w", err)
			}
		}
	}

	// 等待进程退出
	done := make(chan error, 1)
	go func() {
		done <- ch.FfmpegCmd.Wait()
	}()

	// 最多等待 5 秒
	select {
	case <-done:
		ch.Info("ffmpeg 进程已正常退出")
		return nil
	case <-time.After(5 * time.Second):
		ch.Info("ffmpeg 进程未在 5 秒内退出，强制终止")
		if err := ch.FfmpegCmd.Process.Kill(); err != nil {
			return fmt.Errorf("强制终止失败：%w", err)
		}
		return nil
	}
}

// decodeGBK 将 GBK 编码的字节切片转换为 UTF-8 字符串
func decodeGBK(data []byte) string {
	reader := transform.NewReader(bytes.NewReader(data), simplifiedchinese.GBK.NewDecoder())
	decoded, err := io.ReadAll(reader)
	if err != nil {
		// 如果解码失败，返回原始字符串
		return string(data)
	}
	return string(decoded)
}
