package internal

import "errors"

var (
	ErrChannelExists     = errors.New("频道存在")
	ErrChannelNotFound   = errors.New("未找到频道")
	ErrCloudflareBlocked = errors.New("被Cloudflare拦截；请尝试使用-cookies和-user-agent参数")
	ErrAgeVerification   = errors.New("需要年龄验证；请尝试使用-cookies和-user-agent参数")
	ErrChannelOffline    = errors.New("频道离线")
	ErrPrivateStream     = errors.New("频道离线或处在私密状态")
	ErrPaused            = errors.New("暂停频道的录制")
	ErrStopped           = errors.New("停止频道的录制")
)
