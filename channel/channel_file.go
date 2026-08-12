package channel

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/teacat/chaturbate-dvr/server"
)

// Pattern holds the date/time and sequence information for the filename pattern
type Pattern struct {
	Username string
	Year     string
	Month    string
	Day      string
	Hour     string
	Minute   string
	Second   string
	Sequence int
}

// NextFile prepares the next file to be created, by cleaning up the last file and generating a new one
func (ch *Channel) NextFile() error {
	if err := ch.Cleanup(); err != nil {
		return err
	}
	filename, err := ch.GenerateFilename()
	if err != nil {
		return err
	}
	ch.CurrentFilename = filename
	if err := ch.CreateNewFile(filename); err != nil {
		return err
	}

	// Increment the sequence number for the next file
	ch.Sequence++
	return nil
}

// Cleanup cleans the file and resets it, called when the stream errors out or before next file was created.
func (ch *Channel) Cleanup() error {
	if ch.File == nil && ch.AudioFile == nil {
		return nil
	}
	currentFilename := ch.CurrentFilename

	defer func() {
		ch.File = nil
		ch.AudioFile = nil
		ch.CurrentFilename = ""
		ch.Filesize = 0
		ch.Duration = 0
		ch.videoMediaBytes = 0
		ch.audioMediaBytes = 0
	}()

	if ch.RealtimeMux && ch.File != nil {
		if err := ch.syncRealtimeFile(true); err != nil {
			return err
		}
		filename, info, err := closeTrackedFile(ch.File)
		if err != nil {
			return err
		}
		if ch.AudioFile != nil {
			_, _, _ = closeTrackedFile(ch.AudioFile)
		}
		if info != nil && ch.videoMediaBytes > 0 && ch.audioMediaBytes > 0 {
			if ch.Config.Compress {
				finalName, err := moveFileUnique(filename, currentFilename+".mp4")
				if err != nil {
					return fmt.Errorf("为压缩提交实时 MP4：%w", err)
				}
				ch.CompressFile(finalName)
			} else {
				// Fast-start remuxing reads and writes the completed file. Run it
				// in the bounded post-processing pool so the next recording can
				// begin immediately instead of losing stream segments.
				ch.finalizeRealtimeRecording(filename, currentFilename+".mp4")
			}
		} else if info != nil {
			_ = os.Remove(filename)
		}
		return nil
	}

	videoFilename, videoInfo, err := closeTrackedFile(ch.File)
	if err != nil {
		return err
	}
	audioFilename, audioInfo, err := closeTrackedFile(ch.AudioFile)
	if err != nil {
		return err
	}

	if ch.HasSeparateAudio {
		videoHasMedia := hasTrackMedia(videoInfo, ch.videoMediaBytes, len(ch.InitSegment))
		audioHasMedia := hasTrackMedia(audioInfo, ch.audioMediaBytes, len(ch.AudioInitSegment))
		if !videoHasMedia && videoInfo != nil {
			if err := os.Remove(videoFilename); err != nil {
				return fmt.Errorf("删除仅含初始化信息的视频文件：%w", err)
			}
			videoInfo = nil
		}
		if !audioHasMedia && audioInfo != nil {
			if err := os.Remove(audioFilename); err != nil {
				return fmt.Errorf("删除仅含初始化信息的音频文件：%w", err)
			}
			audioInfo = nil
		}

		switch {
		case !videoHasMedia && !audioHasMedia:
			return nil
		case !videoHasMedia || videoInfo == nil:
			ch.Info("合并：缺少视频轨，保留纯音频文件 %s", filepath.Base(audioFilename))
			if ch.Config.Compress {
				ch.CompressFile(audioFilename)
			} else {
				ch.MoveToOutputDir(audioFilename)
			}
			return nil
		case !audioHasMedia || audioInfo == nil:
			ch.Info("合并：缺少音频轨，保留纯视频文件 %s", filepath.Base(videoFilename))
			if ch.Config.Compress {
				ch.CompressFile(videoFilename)
			} else {
				ch.MoveToOutputDir(videoFilename)
			}
			return nil
		}

		temp, err := os.CreateTemp(filepath.Dir(currentFilename), ".mux-*.mp4")
		if err != nil {
			return fmt.Errorf("创建合并临时文件：%w", err)
		}
		tempOutput := temp.Name()
		_ = temp.Close()
		_ = os.Remove(tempOutput)
		defer os.Remove(tempOutput)
		if err := ch.MuxAV(videoFilename, audioFilename, tempOutput); err != nil {
			ch.Info("合并：ffmpeg 合并失败，尝试原生方式：%s", err.Error())
			if nativeErr := ch.MuxAVNative(videoFilename, audioFilename, tempOutput); nativeErr != nil {
				return fmt.Errorf("合并音视频：%w", nativeErr)
			}
		}

		// Sanity-check the muxed file before discarding the sidecars. If the
		// output is missing or implausibly small, keep the sidecars so the
		// user can recover manually (or rerun mux with external tools).
		if ok, reason := muxOutputLooksValid(tempOutput, videoInfo, audioInfo); !ok {
			ch.Error("合并：输出可能损坏（%s），保留源文件 %s 和 %s", reason, filepath.Base(videoFilename), filepath.Base(audioFilename))
			return nil
		}
		finalOutput, err := moveFileUnique(tempOutput, currentFilename+".mp4")
		if err != nil {
			return fmt.Errorf("提交音视频合并输出：%w", err)
		}

		_ = os.Remove(videoFilename)
		_ = os.Remove(audioFilename)

		if ch.Config.Compress {
			ch.CompressFile(finalOutput)
		} else {
			ch.MoveToOutputDir(finalOutput)
		}
		return nil
	}

	if videoInfo != nil && videoInfo.Size() > 0 {
		if ch.Config.Compress {
			ch.CompressFile(videoFilename)
		} else {
			ch.MoveToOutputDir(videoFilename)
		}
	}

	return nil
}

func hasTrackMedia(fileInfo os.FileInfo, trackedMediaBytes, initBytes int) bool {
	if trackedMediaBytes > 0 {
		return true
	}
	if fileInfo == nil {
		return false
	}
	return fileInfo.Size() > int64(initBytes)
}

// muxOutputLooksValid returns true if the muxed MP4 appears to contain most
// of the source bytes. `-c copy` just repackages, so the output should be
// within a reasonable fraction of the combined input size; anything much
// smaller means the muxer bailed out early and the sidecars are more
// valuable than the corrupt result.
func muxOutputLooksValid(outputPath string, videoInfo, audioInfo os.FileInfo) (bool, string) {
	finalInfo, err := os.Stat(outputPath)
	if err != nil {
		return false, fmt.Sprintf("读取文件信息失败：%s", err.Error())
	}
	if finalInfo.Size() == 0 {
		return false, "输出文件为空"
	}
	inputSize := videoInfo.Size() + audioInfo.Size()
	if inputSize == 0 {
		return true, ""
	}
	if finalInfo.Size()*2 < inputSize {
		return false, fmt.Sprintf("输出为 %d 字节，源文件合计 %d 字节", finalInfo.Size(), inputSize)
	}
	return true, ""
}

// MoveToOutputDir relocates a finalized recording into server.Config.OutputDir.
// Errors are non-fatal: the recording is already safely written at srcPath.
func (ch *Channel) MoveToOutputDir(srcPath string) string {
	if server.Config == nil || server.Config.OutputDir == "" {
		return srcPath
	}

	destDir := server.Config.OutputDir
	if server.Config.PerModelFolder {
		destDir = filepath.Join(destDir, ch.Config.Username)
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		ch.Error("输出目录：创建目录 %s 失败：%s", destDir, err.Error())
		return srcPath
	}

	destPath, err := moveFileUnique(srcPath, filepath.Join(destDir, filepath.Base(srcPath)))
	if err != nil {
		ch.Error("输出目录：移动文件 %s 失败：%s", filepath.Base(srcPath), err.Error())
		return srcPath
	}
	ch.Info("输出目录：已移动 %s -> %s", filepath.Base(srcPath), destPath)
	return destPath
}

// uniqueDestPath returns path if it does not exist, otherwise appends
// " (n)" before the extension until an unused path is found. Gives up
// after 1000 tries and returns the last candidate.
func uniqueDestPath(path string) string {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return path
	}
	ext := filepath.Ext(path)
	base := path[:len(path)-len(ext)]
	for i := 1; i < 1000; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
	return fmt.Sprintf("%s (999)%s", base, ext)
}

func moveFile(src, dest string) error {
	if err := os.Link(src, dest); err == nil {
		return os.Remove(src)
	} else if errors.Is(err, os.ErrExist) {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dest)
		return err
	}
	// Sync before close so a crash between close and os.Remove(src) can't
	// leave a truncated destination alongside a deleted source.
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dest)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dest)
		return err
	}
	return os.Remove(src)
}

func moveFileUnique(src, desired string) (string, error) {
	ext := filepath.Ext(desired)
	base := strings.TrimSuffix(desired, ext)
	for i := 0; i < 1000; i++ {
		candidate := desired
		if i > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		if err := moveFile(src, candidate); err == nil {
			return candidate, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("找不到可用的目标文件名：%s", desired)
}

// GenerateFilename creates a filename based on the configured pattern and the current timestamp
func (ch *Channel) GenerateFilename() (string, error) {
	var buf bytes.Buffer

	// Parse the filename pattern defined in the channel's config
	tpl, err := template.New("filename").Parse(ch.Config.Pattern)
	if err != nil {
		return "", fmt.Errorf("文件名格式错误：%w", err)
	}

	// Get the current time based on the Unix timestamp when the stream was started
	t := time.Unix(ch.StreamedAt, 0)
	pattern := &Pattern{
		Username: ch.Config.Username,
		Sequence: ch.Sequence,
		Year:     t.Format("2006"),
		Month:    t.Format("01"),
		Day:      t.Format("02"),
		Hour:     t.Format("15"),
		Minute:   t.Format("04"),
		Second:   t.Format("05"),
	}

	if err := tpl.Execute(&buf, pattern); err != nil {
		return "", fmt.Errorf("执行文件名模板失败：%w", err)
	}
	return buf.String(), nil
}

// CreateNewFile creates a new file for the channel using the given filename
func (ch *Channel) CreateNewFile(filename string) error {
	// Ensure the directory exists before creating the file
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		return fmt.Errorf("创建录像目录：%w", err)
	}

	ch.videoMediaBytes = 0
	ch.audioMediaBytes = 0
	filename = ch.availableRecordingBase(filename)
	ch.CurrentFilename = filename
	if ch.RealtimeMux && len(ch.CombinedInit) > 0 {
		file, err := os.OpenFile(realtimeWorkingPath(filename), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("无法创建实时 MP4：%w", err)
		}
		ch.File = file
		ch.AudioFile = nil
		ch.MuxSequence = 0
		ch.FragmentsSinceSync = 0
		ch.LastFileSync = time.Now()
		n, err := ch.File.Write(ch.CombinedInit)
		if err != nil {
			return fmt.Errorf("写入双轨初始化信息：%w", err)
		}
		ch.Filesize = n
		return nil
	}

	videoPath := ch.videoPath(filename)
	file, err := os.OpenFile(videoPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("无法创建视频文件 %s：%w", filename, err)
	}
	ch.File = file

	if len(ch.InitSegment) > 0 {
		n, err := ch.File.Write(ch.InitSegment)
		if err != nil {
			return fmt.Errorf("写入视频初始化分片：%w", err)
		}
		ch.Filesize += n
	}

	if ch.HasSeparateAudio {
		audioPath := ch.audioPath(filename)
		audioFile, err := os.OpenFile(audioPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			_ = ch.File.Close()
			ch.File = nil
			return fmt.Errorf("无法创建音频文件 %s：%w", filename, err)
		}
		ch.AudioFile = audioFile

		if len(ch.AudioInitSegment) > 0 {
			if _, err := ch.AudioFile.Write(ch.AudioInitSegment); err != nil {
				_ = ch.File.Close()
				_ = ch.AudioFile.Close()
				ch.File = nil
				ch.AudioFile = nil
				return fmt.Errorf("写入音频初始化分片：%w", err)
			}
		}
	}

	return nil
}

// availableRecordingBase keeps every file belonging to one recording in the
// same collision-free namespace. Existing working, sidecar, and final files
// are all treated as occupied and are never opened for append or truncation.
func (ch *Channel) availableRecordingBase(desired string) string {
	for i := 0; i < 1000; i++ {
		candidate := desired
		if i > 0 {
			candidate = fmt.Sprintf("%s (%d)", desired, i)
		}
		paths := []string{
			candidate + ".ts", candidate + ".mp4", candidate + ".mkv",
			candidate + ".video.ts", candidate + ".video.mp4",
			candidate + ".audio.ts", candidate + ".audio.mp4",
			realtimeWorkingPath(candidate),
		}
		occupied := false
		for _, path := range paths {
			if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
				occupied = true
				break
			}
		}
		if !occupied {
			return candidate
		}
	}
	return fmt.Sprintf("%s (%d)", desired, time.Now().UnixNano())
}

func realtimeWorkingPath(base string) string {
	return base + ".recording.mp4"
}

func (ch *Channel) videoPath(filename string) string {
	if ch.HasSeparateAudio {
		ext := ".video.ts"
		if len(ch.InitSegment) > 0 {
			ext = ".video.mp4"
		}
		return filename + ext
	}

	ext := ".ts"
	if len(ch.InitSegment) > 0 {
		ext = ".mp4"
	}
	return filename + ext
}

func (ch *Channel) audioPath(filename string) string {
	ext := ".audio.ts"
	if len(ch.AudioInitSegment) > 0 {
		ext = ".audio.mp4"
	}
	return filename + ext
}

func closeTrackedFile(file *os.File) (string, os.FileInfo, error) {
	if file == nil {
		return "", nil, nil
	}

	filename := file.Name()
	if err := file.Sync(); err != nil && !errors.Is(err, os.ErrClosed) {
		return "", nil, fmt.Errorf("同步录像文件：%w", err)
	}
	if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return "", nil, fmt.Errorf("关闭录像文件：%w", err)
	}

	fileInfo, err := os.Stat(filename)
	if err != nil && !os.IsNotExist(err) {
		return "", nil, fmt.Errorf("检查空录像文件：%w", err)
	}
	if fileInfo != nil && fileInfo.Size() == 0 {
		if err := os.Remove(filename); err != nil {
			return "", nil, fmt.Errorf("删除空录像文件：%w", err)
		}
		fileInfo = nil
	}

	return filename, fileInfo, nil
}

// ShouldSwitchFile determines whether a new file should be created.
func (ch *Channel) ShouldSwitchFile() bool {
	maxFilesizeBytes := ch.Config.MaxFilesize * 1024 * 1024
	maxDurationSeconds := ch.Config.MaxDuration * 60

	return (ch.Duration >= float64(maxDurationSeconds) && ch.Config.MaxDuration > 0) ||
		(ch.Filesize >= maxFilesizeBytes && ch.Config.MaxFilesize > 0)
}
