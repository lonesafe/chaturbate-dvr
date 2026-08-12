package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/r3labs/sse/v2"
	"github.com/teacat/chaturbate-dvr/channel"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/router/view"
	"github.com/teacat/chaturbate-dvr/server"
)

// Manager is responsible for managing channels and their states.
type Manager struct {
	Channels sync.Map
	SSE      *sse.Server
	configMu sync.Mutex
}

// New initializes a new Manager instance with an SSE server.
func New() (*Manager, error) {

	server := sse.New()
	server.SplitData = true

	updateStream := server.CreateStream("updates")
	updateStream.AutoReplay = false

	return &Manager{
		SSE: server,
	}, nil
}

// SaveConfig saves the current channels and state to a JSON file.
func (m *Manager) SaveConfig() error {
	m.configMu.Lock()
	defer m.configMu.Unlock()
	var config []*entity.ChannelConfig

	m.Channels.Range(func(key, value any) bool {
		config = append(config, value.(*channel.Channel).ConfigSnapshot())
		return true
	})

	b, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("编码频道配置：%w", err)
	}
	if err := os.MkdirAll("./conf", 0755); err != nil {
		return fmt.Errorf("创建配置目录：%w", err)
	}
	temp, err := os.CreateTemp("./conf", "channels-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时配置文件：%w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0644); err != nil {
		temp.Close()
		return fmt.Errorf("设置临时配置文件权限：%w", err)
	}
	if _, err := temp.Write(b); err != nil {
		temp.Close()
		return fmt.Errorf("写入临时配置文件：%w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("同步临时配置文件：%w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("关闭临时配置文件：%w", err)
	}
	if err := replaceFile(tempName, "./conf/channels.json"); err != nil {
		return fmt.Errorf("替换频道配置：%w", err)
	}
	return nil
}

// LoadConfig loads the channels from JSON and starts them.
func (m *Manager) LoadConfig() error {
	b, err := os.ReadFile("./conf/channels.json")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取频道配置：%w", err)
	}

	var config []*entity.ChannelConfig
	if err := json.Unmarshal(b, &config); err != nil {
		return fmt.Errorf("解析频道配置：%w", err)
	}
	config, err = prepareLoadedConfigs(config)
	if err != nil {
		return err
	}
	startRecordingRecovery(config)
	for _, conf := range config {
		if _, exists := m.Channels.Load(conf.Username); exists {
			return fmt.Errorf("规范化后频道用户名重复：%q", conf.Username)
		}
	}

	pausedSeq := 0
	seq := 0
	for _, conf := range config {
		ch := channel.New(conf)
		m.Channels.Store(conf.Username, ch)

		if ch.Config.IsPaused {
			ch.Info("频道处于暂停状态，等待恢复")
			ch.StartPausedWatcher(pausedSeq)
			pausedSeq++
			continue
		}
		go ch.Resume(seq)
		seq++
	}
	return nil
}

func startRecordingRecovery(configs []*entity.ChannelConfig) {
	cutoff := time.Now()
	roots := map[string]struct{}{"./videos": {}}
	if server.Config != nil && server.Config.OutputDir != "" {
		roots[server.Config.OutputDir] = struct{}{}
	}
	for _, conf := range configs {
		prefix := conf.Pattern
		if index := strings.Index(prefix, "{{"); index >= 0 {
			prefix = prefix[:index]
		}
		if root := filepath.Dir(prefix); root != "." && root != "" {
			roots[root] = struct{}{}
		}
	}
	go func() {
		for root := range roots {
			if err := channel.RecoverInterruptedRecordingsBefore(root, cutoff); err != nil {
				log.Printf("[错误] 录像恢复 [%s]：%v", root, err)
			}
		}
	}()
}

func prepareLoadedConfigs(configs []*entity.ChannelConfig) ([]*entity.ChannelConfig, error) {
	seen := make(map[string]struct{}, len(configs))
	for _, conf := range configs {
		if conf == nil {
			return nil, fmt.Errorf("频道配置为空")
		}
		conf.Sanitize()
		if conf.Username == "" {
			return nil, fmt.Errorf("规范化后频道用户名为空")
		}
		if _, ok := seen[conf.Username]; ok {
			return nil, fmt.Errorf("规范化后频道用户名重复：%q", conf.Username)
		}
		seen[conf.Username] = struct{}{}
	}
	return configs, nil
}

// CreateChannel starts monitoring an M3U8 stream
func (m *Manager) CreateChannel(conf *entity.ChannelConfig, shouldSave bool) error {
	if conf == nil {
		return fmt.Errorf("必须提供频道配置")
	}
	conf.Sanitize()
	if conf.Username == "" {
		return fmt.Errorf("规范化后频道用户名为空")
	}
	ch := channel.New(conf)

	// prevent duplicate channels
	if _, loaded := m.Channels.LoadOrStore(conf.Username, ch); loaded {
		ch.Stop()
		return fmt.Errorf("频道 %s 已添加", conf.Username)
	}

	if shouldSave {
		if err := m.SaveConfig(); err != nil {
			m.Channels.Delete(conf.Username)
			ch.Stop()
			return fmt.Errorf("保存频道配置：%w", err)
		}
	}
	ch.Resume(0)
	return nil
}

// StopChannel stops the channel.
func (m *Manager) StopChannel(username string) error {
	thing, ok := m.Channels.Load(username)
	if !ok {
		return nil
	}
	m.Channels.Delete(username)

	if err := m.SaveConfig(); err != nil {
		m.Channels.Store(username, thing)
		return fmt.Errorf("保存频道配置：%w", err)
	}
	thing.(*channel.Channel).Stop()
	return nil
}

// PauseChannel pauses the channel.
func (m *Manager) PauseChannel(username string) error {
	thing, ok := m.Channels.Load(username)
	if !ok {
		return nil
	}
	ch := thing.(*channel.Channel)
	wasPaused := ch.ConfigSnapshot().IsPaused
	ch.Pause()

	if err := m.SaveConfig(); err != nil {
		if !wasPaused {
			ch.Resume(0)
		}
		return fmt.Errorf("保存频道配置：%w", err)
	}
	return nil
}

// ResumeChannel resumes the channel.
func (m *Manager) ResumeChannel(username string) error {
	thing, ok := m.Channels.Load(username)
	if !ok {
		return nil
	}
	ch := thing.(*channel.Channel)
	wasPaused := ch.ConfigSnapshot().IsPaused
	ch.Resume(0)

	if err := m.SaveConfig(); err != nil {
		if wasPaused {
			ch.Pause()
		}
		return fmt.Errorf("保存频道配置：%w", err)
	}
	return nil
}

// ChannelInfo returns a list of channel information for the web UI.
func (m *Manager) ChannelInfo() []*entity.ChannelInfo {
	var channels []*entity.ChannelInfo

	// Iterate over the channels and append their information to the slice
	m.Channels.Range(func(key, value any) bool {
		channels = append(channels, value.(*channel.Channel).ExportInfo())
		return true
	})

	sort.Slice(channels, func(i, j int) bool {
		// Paused channels always sort to the bottom.
		getPriority := func(c *entity.ChannelInfo) int {
			switch {
			case !c.IsPaused && c.IsOnline:
				return 0 // Recording
			case !c.IsPaused:
				return 1 // Offline, actively watching
			case c.IsOnline:
				return 2 // Paused, currently online
			default:
				return 3 // Paused, offline
			}
		}

		pi, pj := getPriority(channels[i]), getPriority(channels[j])
		if pi != pj {
			return pi < pj
		}
		// Same priority: sort by username alphabetically
		return strings.ToLower(channels[i].Username) < strings.ToLower(channels[j].Username)
	})

	return channels
}

// Publish sends an SSE event to the specified channel.
func (m *Manager) Publish(evt entity.Event, info *entity.ChannelInfo) {
	switch evt {
	case entity.EventUpdate:
		var b bytes.Buffer
		if err := view.InfoTpl.ExecuteTemplate(&b, "channel_info", info); err != nil {
			log.Printf("[错误] 渲染频道信息模板失败：%v", err)
			return
		}
		m.SSE.Publish("updates", &sse.Event{
			Event: []byte(info.Username + "-info"),
			Data:  b.Bytes(),
		})
	case entity.EventLog:
		m.SSE.Publish("updates", &sse.Event{
			Event: []byte(info.Username + "-log"),
			Data:  []byte(strings.Join(info.Logs, "\n")),
		})
	}
}

// Subscriber handles SSE subscriptions for the specified channel.
func (m *Manager) Subscriber(w http.ResponseWriter, r *http.Request) {
	m.SSE.ServeHTTP(w, r)
}

func (m *Manager) Shutdown(ctx context.Context) error {
	var channels []*channel.Channel
	m.Channels.Range(func(_, value any) bool {
		channels = append(channels, value.(*channel.Channel))
		return true
	})
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, ch := range channels {
			wg.Add(1)
			go func(ch *channel.Channel) {
				defer wg.Done()
				ch.Stop()
			}(ch)
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return m.SaveConfig()
	case <-ctx.Done():
		return fmt.Errorf("等待优雅关闭超时：%w", ctx.Err())
	}
}
