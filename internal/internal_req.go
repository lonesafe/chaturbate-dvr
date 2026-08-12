package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/teacat/chaturbate-dvr/server"
)

// HTTPStatusError reports a non-2xx HTTP response while preserving its status
// code for callers that need status-specific retry behavior.
type HTTPStatusError struct {
	StatusCode int
	URL        string
	Cause      error
	RetryAfter time.Duration
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("请求 %s 返回异常 HTTP 状态码 %d", e.URL, e.StatusCode)
}

func (e *HTTPStatusError) Unwrap() error {
	return e.Cause
}

// IsHTTPStatus reports whether err contains an HTTPStatusError with one of the
// supplied status codes.
func IsHTTPStatus(err error, statusCodes ...int) bool {
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	for _, statusCode := range statusCodes {
		if statusErr.StatusCode == statusCode {
			return true
		}
	}
	return false
}

func HTTPRetryAfter(err error) time.Duration {
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return statusErr.RetryAfter
	}
	return 0
}

func HTTPStatusCode(err error) int {
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode
	}
	return 0
}

// Req represents an HTTP client with customized settings.
type Req struct {
	client *http.Client
}

const (
	maxTextResponseBytes   int64 = 4 << 20  // API and M3U8 responses
	maxBinaryResponseBytes int64 = 64 << 20 // init and media segments
)

var (
	sharedClientOnce sync.Once
	sharedClient     *http.Client
)

// NewReq creates a new HTTP client with specific transport configurations.
func NewReq() *Req {
	sharedClientOnce.Do(func() {
		sharedClient = &http.Client{Transport: CreateTransport()}
	})
	return &Req{client: sharedClient}
}

// CreateTransport initializes a custom HTTP transport.
func CreateTransport() *http.Transport {
	// The DefaultTransport allows user changes the proxy settings via environment variables
	// such as HTTP_PROXY, HTTPS_PROXY.
	defaultTransport := http.DefaultTransport.(*http.Transport)

	return defaultTransport.Clone()
}

func requestLimits(binary bool) (int64, time.Duration) {
	limit, timeout := maxTextResponseBytes, 30*time.Second
	if binary {
		limit, timeout = maxBinaryResponseBytes, 2*time.Minute
	}
	if server.Config != nil {
		if binary {
			if server.Config.MaxSegmentMB > 0 {
				limit = int64(server.Config.MaxSegmentMB) << 20
			}
			if server.Config.SegmentTimeoutSeconds > 0 {
				timeout = time.Duration(server.Config.SegmentTimeoutSeconds) * time.Second
			}
		} else {
			if server.Config.MaxTextMB > 0 {
				limit = int64(server.Config.MaxTextMB) << 20
			}
			if server.Config.HTTPTimeoutSeconds > 0 {
				timeout = time.Duration(server.Config.HTTPTimeoutSeconds) * time.Second
			}
		}
	}
	return limit, timeout
}

// Get sends an HTTP GET request and returns the response as a string.
func (h *Req) Get(ctx context.Context, url string) (string, error) {
	limit, timeout := requestLimits(false)
	resp, err := h.getBytes(ctx, url, limit, timeout)
	if err != nil {
		return "", fmt.Errorf("读取响应内容：%w", err)
	}
	return string(resp), nil
}

// GetBytes sends an HTTP GET request and returns the response as a byte slice.
func (h *Req) GetBytes(ctx context.Context, url string) ([]byte, error) {
	limit, timeout := requestLimits(true)
	return h.getBytes(ctx, url, limit, timeout)
}

func (h *Req) getBytes(ctx context.Context, url string, maxBytes int64, timeout time.Duration) ([]byte, error) {
	req, cancel, err := createRequest(ctx, url, timeout)
	if err != nil {
		return nil, fmt.Errorf("创建 HTTP 请求：%w", err)
	}
	defer cancel()

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发送 HTTP 请求：%w", err)
	}
	defer resp.Body.Close()
	if resp.ContentLength > maxBytes {
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return nil, newHTTPStatusError(resp, req.URL.String(), nil)
		}
		return nil, fmt.Errorf("%w：Content-Length 为 %d 字节，限制为 %d 字节", ErrResponseTooLarge, resp.ContentLength, maxBytes)
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取 HTTP 响应：%w", err)
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("%w：实际接收超过 %d 字节", ErrResponseTooLarge, maxBytes)
	}

	var responseErr error
	if strings.Contains(string(b), "<title>Just a moment...</title>") {
		responseErr = ErrCloudflareBlocked
	} else if strings.Contains(string(b), "Verify your age") {
		responseErr = ErrAgeVerification
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, newHTTPStatusError(resp, req.URL.String(), responseErr)
	}
	if responseErr != nil {
		return nil, responseErr
	}

	return b, err
}

func newHTTPStatusError(resp *http.Response, requestURL string, cause error) *HTTPStatusError {
	retryAfter := time.Duration(0)
	if value := resp.Header.Get("Retry-After"); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
			retryAfter = time.Duration(seconds) * time.Second
		} else if when, err := http.ParseTime(value); err == nil {
			retryAfter = time.Until(when)
			if retryAfter < 0 {
				retryAfter = 0
			}
		}
	}
	return &HTTPStatusError{StatusCode: resp.StatusCode, URL: requestURL, Cause: cause, RetryAfter: retryAfter}
}

// Head sends an HTTP HEAD request and returns the status code.
func (h *Req) Head(ctx context.Context, url string) (int, error) {
	_, timeout := requestLimits(false)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return 0, err
	}
	SetRequestHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	return resp.StatusCode, nil
}

// CreateRequest constructs an HTTP GET request with necessary headers.
func CreateRequest(ctx context.Context, url string) (*http.Request, context.CancelFunc, error) {
	_, timeout := requestLimits(false)
	return createRequest(ctx, url, timeout)
}

func createRequest(ctx context.Context, url string, timeout time.Duration) (*http.Request, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, cancel, err
	}
	SetRequestHeaders(req)
	return req, cancel, nil
}

// SetRequestHeaders applies necessary headers to the request.
func SetRequestHeaders(req *http.Request) {
	req.Header.Set("X-Requested-With", "XMLHttpRequest") // So Cloudflare would likely accept the request, and no Age Verification

	if server.Config == nil {
		return
	}
	server.ConfigMu.RLock()
	userAgent := server.Config.UserAgent
	cookieString := server.Config.Cookies
	server.ConfigMu.RUnlock()
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	if cookieString != "" {
		cookies := ParseCookies(cookieString)
		for name, value := range cookies {
			req.AddCookie(&http.Cookie{Name: name, Value: value})
		}
	}
}

// ParseCookies converts a cookie string into a map.
func ParseCookies(cookieStr string) map[string]string {
	cookies := make(map[string]string)
	pairs := strings.Split(cookieStr, ";")

	// Iterate over each cookie pair and extract key-value pairs
	for _, pair := range pairs {
		parts := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(parts) == 2 {
			// Trim spaces around key and value
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			// Store cookie name and value in the map
			cookies[key] = value
		}
	}
	return cookies
}
