package internal

import "errors"

var (
	ErrChannelExists     = errors.New("频道已存在")
	ErrChannelNotFound   = errors.New("频道不存在")
	ErrCloudflareBlocked = errors.New("被 Cloudflare 拦截；请检查 Cookies 和 User-Agent")
	ErrAgeVerification   = errors.New("需要完成年龄验证；请检查 Cookies 和 User-Agent")
	ErrChannelOffline    = errors.New("频道已离线")
	ErrPrivateStream     = errors.New("频道已离线或进入私密直播")
	ErrPaused            = errors.New("频道已暂停")
	ErrStopped           = errors.New("频道已停止")
	ErrGeoBlocked        = errors.New("无法访问直播流（可能受地区限制）")
	ErrPlaylistForbidden = errors.New("播放列表不可用，需要刷新直播地址")
	ErrResponseTooLarge  = errors.New("HTTP 响应超过大小限制")
	ErrTimelineReset     = errors.New("直播时间轴或初始化信息已变化")
	ErrDiskSpace         = errors.New("磁盘剩余空间不足")
)
