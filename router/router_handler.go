package router

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	appconfig "github.com/teacat/chaturbate-dvr/config"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/server"
)

// IndexData represents the data structure for the index page.
type IndexData struct {
	Config   *entity.Config
	Channels []*entity.ChannelInfo
}

// Index renders the index page with channel information.
func Index(c *gin.Context) {
	c.HTML(200, "index.html", &IndexData{
		Config:   server.Config,
		Channels: server.Manager.ChannelInfo(),
	})
}

// CreateChannelRequest represents the request body for creating a channel.
type CreateChannelRequest struct {
	Username string `form:"username" binding:"required"`
}

type CreateChannelResult struct {
	Created []string `json:"created"`
	Skipped []string `json:"skipped"`
}

// CreateChannel creates a new channel.
func CreateChannel(c *gin.Context) {
	var req *CreateChannelRequest
	if err := c.Bind(&req); err != nil {
		c.AbortWithError(http.StatusBadRequest, fmt.Errorf("解析请求：%w", err))
		return
	}
	server.ConfigMu.RLock()
	framerate, resolution, pattern := server.Config.Framerate, server.Config.Resolution, server.Config.Pattern
	maxDuration, maxFilesize, compress := server.Config.MaxDuration, server.Config.MaxFilesize, server.Config.Compress
	server.ConfigMu.RUnlock()

	usernames, duplicateInputs, err := normalizeUsernames(req.Username)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	existing := make(map[string]struct{})
	for _, info := range server.Manager.ChannelInfo() {
		existing[info.Username] = struct{}{}
	}
	skipped := make([]string, 0, len(duplicateInputs))
	skippedSet := make(map[string]struct{}, len(duplicateInputs))
	for _, username := range duplicateInputs {
		if _, ok := skippedSet[username]; !ok {
			skipped = append(skipped, username)
			skippedSet[username] = struct{}{}
		}
	}
	created := make([]string, 0, len(usernames))
	for _, username := range usernames {
		if _, ok := existing[username]; ok {
			if _, alreadySkipped := skippedSet[username]; !alreadySkipped {
				skipped = append(skipped, username)
				skippedSet[username] = struct{}{}
			}
			continue
		}
		if err := server.Manager.CreateChannel(&entity.ChannelConfig{
			IsPaused:  false,
			Username:  username,
			Framerate: framerate, Resolution: resolution, Pattern: pattern,
			MaxDuration: maxDuration, MaxFilesize: maxFilesize, Compress: compress,
			CreatedAt: time.Now().Unix(),
		}, true); err != nil {
			for i := len(created) - 1; i >= 0; i-- {
				_ = server.Manager.StopChannel(created[i])
			}
			c.String(http.StatusConflict, "添加频道 %s 失败：%s", username, err.Error())
			return
		}
		created = append(created, username)
	}
	c.JSON(http.StatusOK, CreateChannelResult{Created: created, Skipped: skipped})
}

func normalizeUsernames(input string) ([]string, []string, error) {
	parts := strings.FieldsFunc(input, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ';' || r == '；' || r == '，'
	})
	usernames := make([]string, 0, len(parts))
	duplicates := make([]string, 0)
	seen := make(map[string]struct{}, len(parts))
	for _, value := range parts {
		conf := &entity.ChannelConfig{Username: value}
		conf.Sanitize()
		if conf.Username == "" {
			continue
		}
		if _, ok := seen[conf.Username]; ok {
			duplicates = append(duplicates, conf.Username)
			continue
		}
		seen[conf.Username] = struct{}{}
		usernames = append(usernames, conf.Username)
	}
	if len(usernames) == 0 {
		return nil, nil, fmt.Errorf("请至少输入一个有效的频道用户名")
	}
	return usernames, duplicates, nil
}

// StopChannel stops a channel.
func StopChannel(c *gin.Context) {
	if err := server.Manager.StopChannel(c.Param("username")); err != nil {
		c.String(http.StatusInternalServerError, "停止频道失败：%s", err.Error())
		return
	}

	c.Redirect(http.StatusFound, "/")
}

// PauseChannel pauses a channel.
func PauseChannel(c *gin.Context) {
	if err := server.Manager.PauseChannel(c.Param("username")); err != nil {
		c.String(http.StatusInternalServerError, "暂停频道失败：%s", err.Error())
		return
	}

	c.Redirect(http.StatusFound, "/")
}

// ResumeChannel resumes a paused channel.
func ResumeChannel(c *gin.Context) {
	if err := server.Manager.ResumeChannel(c.Param("username")); err != nil {
		c.String(http.StatusInternalServerError, "恢复频道失败：%s", err.Error())
		return
	}

	c.Redirect(http.StatusFound, "/")
}

func safeRecordingPattern(pattern string) bool {
	if pattern == "" || absoluteRecordingPattern(pattern) || containsParentPathComponent(pattern) {
		return false
	}
	clean := filepath.Clean(strings.ReplaceAll(pattern, `\`, "/"))
	return clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func absoluteRecordingPattern(pattern string) bool {
	return filepath.IsAbs(pattern) || strings.HasPrefix(pattern, "/") || strings.HasPrefix(pattern, `\`) || (len(pattern) >= 2 && pattern[1] == ':')
}

func containsParentPathComponent(pattern string) bool {
	for _, component := range strings.Split(strings.ReplaceAll(pattern, `\`, "/"), "/") {
		if component == ".." {
			return true
		}
	}
	return false
}

// trustedAbsoluteRecordingPattern keeps an absolute recording root loaded
// from CLI or persisted configuration usable from the Web UI. It never lets a
// Web request expand that authority to another directory. In particular, a
// deployment already configured for /downloads may edit the filename portion
// below /downloads, but cannot redirect recordings to /etc or another root.
func trustedAbsoluteRecordingPattern(pattern, currentPattern string) bool {
	if !filepath.IsAbs(pattern) || containsParentPathComponent(pattern) {
		return false
	}
	trustedRoot, ok := absolutePatternRoot(currentPattern)
	if !ok {
		return false
	}
	candidate := filepath.Clean(pattern)
	relative, err := filepath.Rel(trustedRoot, candidate)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

func absolutePatternRoot(pattern string) (string, bool) {
	if !filepath.IsAbs(pattern) || containsParentPathComponent(pattern) {
		return "", false
	}
	staticPrefix := pattern
	if templateStart := strings.Index(staticPrefix, "{{"); templateStart >= 0 {
		staticPrefix = staticPrefix[:templateStart]
	}
	if staticPrefix == "" {
		return "", false
	}
	root := filepath.Clean(staticPrefix)
	if !strings.HasSuffix(staticPrefix, string(filepath.Separator)) && !strings.HasSuffix(staticPrefix, "/") && !strings.HasSuffix(staticPrefix, `\`) {
		root = filepath.Dir(root)
	}
	volumeRoot := filepath.Clean(filepath.VolumeName(root) + string(filepath.Separator))
	if !filepath.IsAbs(root) || root == volumeRoot {
		return "", false
	}
	return root, true
}

func allowedRecordingPattern(pattern, currentPattern string) bool {
	return safeRecordingPattern(pattern) || trustedAbsoluteRecordingPattern(pattern, currentPattern)
}

// Updates handles the SSE connection for updates.
func Updates(c *gin.Context) {
	server.Manager.Subscriber(c.Writer, c.Request)
}

// UpdateConfigRequest represents the request body for updating configuration.
type UpdateConfigRequest struct {
	Cookies         string `form:"cookies"`
	UserAgent       string `form:"user_agent"`
	Framerate       int    `form:"framerate" binding:"required,oneof=30 60"`
	Resolution      int    `form:"resolution" binding:"required,oneof=240 480 540 720 1080 1440 2160"`
	Pattern         string `form:"pattern" binding:"required"`
	MaxDuration     int    `form:"max_duration" binding:"min=0"`
	MaxFilesize     int    `form:"max_filesize" binding:"min=0"`
	Compress        bool   `form:"compress"`
	PairToleranceMS int    `form:"pair_tolerance_ms" binding:"required,min=1,max=5000"`
}

// UpdateConfig updates the server configuration.
func UpdateConfig(c *gin.Context) {
	var req *UpdateConfigRequest
	if err := c.Bind(&req); err != nil {
		c.AbortWithError(http.StatusBadRequest, fmt.Errorf("解析请求：%w", err))
		return
	}

	server.ConfigMu.Lock()
	if !allowedRecordingPattern(req.Pattern, server.Config.Pattern) {
		server.ConfigMu.Unlock()
		c.String(http.StatusBadRequest, "文件名格式不能包含 '..'；绝对路径只能使用当前已配置的录制根目录")
		return
	}
	previous := *server.Config
	server.Config.Cookies = req.Cookies
	server.Config.UserAgent = req.UserAgent
	server.Config.Framerate, server.Config.Resolution, server.Config.Pattern = req.Framerate, req.Resolution, req.Pattern
	server.Config.MaxDuration, server.Config.MaxFilesize, server.Config.Compress = req.MaxDuration, req.MaxFilesize, req.Compress
	server.Config.PairToleranceMS = req.PairToleranceMS
	if err := appconfig.SaveGlobalSettings(server.Config); err != nil {
		*server.Config = previous
		server.ConfigMu.Unlock()
		c.String(http.StatusInternalServerError, "保存设置失败：%s", err.Error())
		return
	}
	server.ConfigMu.Unlock()
	c.Redirect(http.StatusFound, "/")
}
