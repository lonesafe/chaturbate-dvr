package chaturbate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/grafov/m3u8"
	"github.com/samber/lo"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
)

// roomDossierRegexp is used to extract the room dossier information from the HTML response.
var roomDossierRegexp = regexp.MustCompile(`window\.initialRoomDossier = "(.*?)"`)

// Client represents an API client for interacting with Chaturbate.
type Client struct {
	Req *internal.Req
}

// NewClient initializes and returns a new Client instance.
func NewClient() *Client {
	return &Client{
		Req: internal.NewReq(),
	}
}

// GetStream 获取指定用户的流信息
func (c *Client) GetStream(ctx context.Context, username string) (*Stream, error) {
	return FetchStream(ctx, c.Req, username)
}

// FetchStream 从指定用户页面获取流数据.
func FetchStream(ctx context.Context, client *internal.Req, username string) (*Stream, error) {
	body, err := client.Get(ctx, fmt.Sprintf("%s//api/chatvideocontext/%s", server.Config.Domain, username))
	if err != nil {
		return nil, fmt.Errorf("获取页面正文失败: %w", err)
	}

	return ParseStream(body)
}

// ParseStream 从给定页面正文中提取HLS源URL
func ParseStream(body string) (*Stream, error) {
	// 解析 JSON 响应
	var roomData struct {
		RoomStatus string `json:"room_status"`
		HLSSource  string `json:"hls_source"`
	}
	if err := json.Unmarshal([]byte(body), &roomData); err != nil {
		return nil, fmt.Errorf("解析 JSON 失败：%w", err)
	}

	// 检查房间状态
	if roomData.RoomStatus != "public" {
		return nil, internal.ErrChannelOffline
	}

	// 检查 HLS 源是否存在
	if roomData.HLSSource == "" {
		return nil, internal.ErrChannelOffline
	}

	return &Stream{HLSSource: roomData.HLSSource}, nil
}

// Stream 代表 HLS 流源
type Stream struct {
	HLSSource string
}

// GetPlaylist 获取与指定分辨率和帧率对应的播放列表
func (s *Stream) GetPlaylist(ctx context.Context, resolution, framerate int) (*Playlist, error) {
	return FetchPlaylist(ctx, s.HLSSource, resolution, framerate)
}

// FetchPlaylist 获取并解码 HLS 播放列表文件
func FetchPlaylist(ctx context.Context, hlsSource string, resolution, framerate int) (*Playlist, error) {
	if hlsSource == "" {
		return nil, errors.New("HLS 源为空")
	}

	resp, err := internal.NewReq().Get(ctx, hlsSource)
	if err != nil {
		return nil, fmt.Errorf("获取 HLS 源失败：%w", err)
	}

	return ParsePlaylist(resp, hlsSource, resolution, framerate)
}

// ParsePlaylist decodes the M3U8 playlist and extracts the variant streams.
func ParsePlaylist(resp, hlsSource string, resolution, framerate int) (*Playlist, error) {
	p, _, err := m3u8.DecodeFrom(strings.NewReader(resp), true)
	if err != nil {
		return nil, fmt.Errorf("解码m3u8播放列表失败: %w", err)
	}

	masterPlaylist, ok := p.(*m3u8.MasterPlaylist)
	if !ok {
		return nil, errors.New("无效的主播放列表格式")
	}

	return PickPlaylist(masterPlaylist, hlsSource, resolution, framerate)
}

// Playlist represents an HLS playlist containing variant streams.
type Playlist struct {
	PlaylistURL string
	RootURL     string
	Resolution  int
	Framerate   int
}

// Resolution represents a video resolution and its corresponding framerate.
type Resolution struct {
	Framerate map[int]string // [framerate]url
	Width     int
}

// PickPlaylist selects the best matching variant stream based on resolution and framerate.
func PickPlaylist(masterPlaylist *m3u8.MasterPlaylist, baseURL string, resolution, framerate int) (*Playlist, error) {
	resolutions := map[int]*Resolution{}

	// Extract available resolutions and framerates from the master playlist
	for _, v := range masterPlaylist.Variants {
		parts := strings.Split(v.Resolution, "x")
		if len(parts) != 2 {
			continue
		}
		width, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("解析分辨率：%w", err)
		}
		framerateVal := 30
		if strings.Contains(v.Name, "FPS:60.0") {
			framerateVal = 60
		}
		if _, exists := resolutions[width]; !exists {
			resolutions[width] = &Resolution{Framerate: map[int]string{}, Width: width}
		}
		resolutions[width].Framerate[framerateVal] = v.URI
	}

	// Find exact match for requested resolution
	variant, exists := resolutions[resolution]
	if !exists {
		// Filter resolutions below the requested resolution
		candidates := lo.Filter(lo.Values(resolutions), func(r *Resolution, _ int) bool {
			return r.Width < resolution
		})
		// Pick the highest resolution among the candidates
		variant = lo.MaxBy(candidates, func(a, b *Resolution) bool {
			return a.Width > b.Width
		})
	}
	if variant == nil {
		return nil, fmt.Errorf("未找到分辨率")
	}

	var (
		finalResolution = variant.Width
		finalFramerate  = framerate
	)
	// Select the desired framerate, or fallback to the first available framerate
	playlistURL, exists := variant.Framerate[framerate]
	if !exists {
		for fr, url := range variant.Framerate {
			playlistURL = url
			finalFramerate = fr
			break
		}
	}

	// 使用 url.Parse 和 ResolveReference 正确处理 M3U8 URL 拼接
	baseURLParsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("解析基础 URL 失败：%w", err)
	}

	playlistURLParsed, err := url.Parse(playlistURL)
	if err != nil {
		return nil, fmt.Errorf("解析播放列表 URL 失败：%w", err)
	}

	// ResolveReference 会自动处理相对路径和绝对路径
	fullPlaylistURL := baseURLParsed.ResolveReference(playlistURLParsed).String()

	// 获取根目录 URL（移除文件名部分）
	rootURL := baseURL[:strings.LastIndexByte(baseURL, '/')+1]

	return &Playlist{
		PlaylistURL: fullPlaylistURL,
		RootURL:     rootURL,
		Resolution:  finalResolution,
		Framerate:   finalFramerate,
	}, nil
}

// buildSegmentURL 构建视频分片的完整 URL
func buildSegmentURL(rootURL, segmentURI string) (string, error) {
	rootURLParsed, err := url.Parse(rootURL)
	if err != nil {
		return "", fmt.Errorf("解析根 URL 失败：%w", err)
	}

	segmentURLParsed, err := url.Parse(segmentURI)
	if err != nil {
		return "", fmt.Errorf("解析分片 URI 失败：%w", err)
	}

	return rootURLParsed.ResolveReference(segmentURLParsed).String(), nil
}

// WatchHandler is a function type that processes video segments.
type WatchHandler func(b []byte, duration float64) error

// WatchSegments continuously fetches and processes video segments.
func (p *Playlist) WatchSegments(ctx context.Context, handler WatchHandler) error {
	client := internal.NewReq()
	processedURIs := make(map[string]bool)

	for {
		resp, err := client.Get(ctx, p.PlaylistURL)
		if err != nil {
			return fmt.Errorf("获取播放列表：%w", err)
		}
		pl, _, err := m3u8.DecodeFrom(strings.NewReader(resp), true)
		if err != nil {
			return fmt.Errorf("解码 M3U8: %w", err)
		}
		playlist, ok := pl.(*m3u8.MediaPlaylist)
		if !ok {
			return errors.New("无效的媒体播放列表格式")
		}

		var newSegments []*m3u8.MediaSegment
		for _, v := range playlist.Segments {
			if v == nil {
				continue
			}
			if !processedURIs[v.URI] {
				newSegments = append(newSegments, v)
				processedURIs[v.URI] = true
			}
		}

		for _, segment := range newSegments {
			pipeline := func() ([]byte, error) {
				fullURL, err := buildSegmentURL(p.RootURL, segment.URI)
				if err != nil {
					return nil, err
				}
				return client.GetBytes(ctx, fullURL)
			}

			resp, err := retry.DoWithData(
				pipeline,
				retry.Context(ctx),
				retry.Attempts(3),
				retry.Delay(600*time.Millisecond),
				retry.DelayType(retry.FixedDelay),
			)
			if err != nil {
				break
			}

			if err := handler(resp, segment.Duration); err != nil {
				return fmt.Errorf("处理分片失败：%w", err)
			}
		}

		time.Sleep(1 * time.Second)
	}
}

// GetInitSegment 获取 init segment（如果存在）
func (p *Playlist) GetInitSegment(ctx context.Context) ([]byte, error) {
	resp, err := internal.NewReq().Get(ctx, p.PlaylistURL)
	if err != nil {
		return nil, fmt.Errorf("获取播放列表失败：%w", err)
	}

	pl, _, err := m3u8.DecodeFrom(strings.NewReader(resp), true)
	if err != nil {
		return nil, fmt.Errorf("解码 M3U8 失败：%w", err)
	}

	mediaPlaylist, ok := pl.(*m3u8.MediaPlaylist)
	if !ok {
		return nil, errors.New("无效的媒体播放列表格式")
	}

	// 检查是否有 MAP 标签
	if mediaPlaylist.Map != nil && mediaPlaylist.Map.URI != "" {
		initURL, err := buildSegmentURL(p.RootURL, mediaPlaylist.Map.URI)
		if err != nil {
			return nil, fmt.Errorf("构建 init URL 失败：%w", err)
		}

		return internal.NewReq().GetBytes(ctx, initURL)
	}

	return nil, errors.New("未找到 init segment")
}
