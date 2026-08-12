package chaturbate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grafov/m3u8"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
)

func TestPickPlaylistIncludesDefaultAudioRendition(t *testing.T) {
	t.Parallel()

	master := &m3u8.MasterPlaylist{
		Variants: []*m3u8.Variant{
			{
				URI: "video.m3u8",
				VariantParams: m3u8.VariantParams{
					Resolution: "1920x1080",
					FrameRate:  60,
					Audio:      "audio-main",
					Alternatives: []*m3u8.Alternative{
						{Type: "AUDIO", GroupId: "audio-main", URI: "audio-en.m3u8", Name: "English", Default: true},
					},
				},
			},
		},
	}

	playlist, err := PickPlaylist(master, "https://example.com/master.m3u8", 1080, 60)
	if err != nil {
		t.Fatalf("PickPlaylist() error = %v", err)
	}
	if got, want := playlist.PlaylistURL, "https://example.com/video.m3u8"; got != want {
		t.Fatalf("PlaylistURL = %q, want %q", got, want)
	}
	if got, want := playlist.AudioPlaylistURL, "https://example.com/audio-en.m3u8"; got != want {
		t.Fatalf("AudioPlaylistURL = %q, want %q", got, want)
	}
}

func TestProcessMediaPlaylistSkipsForbiddenSegmentWithoutRetry(t *testing.T) {
	if server.Config == nil {
		server.Config = &entity.Config{}
	}

	playlistBody := strings.Join([]string{
		"#EXTM3U", "#EXT-X-VERSION:3", "#EXT-X-TARGETDURATION:2",
		"#EXT-X-MEDIA-SEQUENCE:100", "#EXTINF:2.000,",
		"seg_1_100_video_x.m4s", "#EXTINF:2.000,",
		"seg_2_101_video_x.m4s", "",
	}, "\n")
	var forbiddenRequests, handlerCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(playlistBody)) })
	mux.HandleFunc("/seg_1_100_video_x.m4s", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&forbiddenRequests, 1)
		http.Error(w, "expired", http.StatusForbidden)
	})
	mux.HandleFunc("/seg_2_101_video_x.m4s", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pl := &Playlist{PlaylistURL: srv.URL + "/playlist.m3u8"}
	lastSeq, initWritten := -1, false
	_, err := pl.processMediaPlaylist(context.Background(), internal.NewReq(), pl.PlaylistURL,
		func(_ []byte, _ float64) error { atomic.AddInt32(&handlerCalls, 1); return nil }, nil, &lastSeq, &initWritten)
	if err != nil {
		t.Fatalf("processMediaPlaylist() error = %v", err)
	}
	if got := atomic.LoadInt32(&forbiddenRequests); got != 1 {
		t.Fatalf("403 segment requested %d times, want 1", got)
	}
	if got := atomic.LoadInt32(&handlerCalls); got != 1 {
		t.Fatalf("handler called %d times, want 1 for the following valid segment", got)
	}
	if lastSeq != 101 {
		t.Fatalf("lastSeq = %d, want 101", lastSeq)
	}
}

func TestProcessMediaPlaylistForbiddenRequestsStreamRefresh(t *testing.T) {
	if server.Config == nil {
		server.Config = &entity.Config{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "expired session", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	pl := &Playlist{PlaylistURL: srv.URL + "/playlist.m3u8"}
	lastSeq, initWritten := -1, false
	_, err := pl.processMediaPlaylist(context.Background(), internal.NewReq(), pl.PlaylistURL, nil, nil, &lastSeq, &initWritten)
	if !errors.Is(err, internal.ErrPlaylistForbidden) {
		t.Fatalf("error = %v, want ErrPlaylistForbidden", err)
	}
}

func TestProcessMediaPlaylistUsesMediaSequenceNotURIFormat(t *testing.T) {
	if server.Config == nil {
		server.Config = &entity.Config{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:42\n#EXTINF:2,\nopaque-name.m4s\n"))
	})
	mux.HandleFunc("/opaque-name.m4s", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("data")) })
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	lastSeq, initialized, calls := -1, false, 0
	p := &Playlist{PlaylistURL: httpServer.URL + "/playlist.m3u8"}
	_, err := p.processMediaPlaylist(context.Background(), internal.NewReq(), p.PlaylistURL, func(_ []byte, _ float64) error { calls++; return nil }, nil, &lastSeq, &initialized)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || lastSeq != 42 {
		t.Fatalf("calls/lastSeq = %d/%d, want 1/42", calls, lastSeq)
	}
}

// A segment that still fails after three attempts is skipped, and processing
// continues with later segments from the same playlist.
func TestProcessMediaPlaylistSkipsFetchFailureAfterRetries(t *testing.T) {
	if server.Config == nil {
		server.Config = &entity.Config{}
	}

	playlistBody := strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"#EXT-X-TARGETDURATION:2",
		"#EXT-X-MEDIA-SEQUENCE:100",
		"#EXTINF:2.000,",
		"seg_1_100_video_abc.m4s",
		"#EXTINF:2.000,",
		"seg_2_101_video_abc.m4s",
		"#EXTINF:2.000,",
		"seg_3_102_video_abc.m4s",
		"",
	}, "\n")

	var handlerCalls, failedRequests int32

	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(playlistBody))
	})
	mux.HandleFunc("/seg_1_100_video_abc.m4s", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("seg-100-data"))
	})
	mux.HandleFunc("/seg_2_101_video_abc.m4s", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&failedRequests, 1)
		// Close the connection mid-response so the client gets a real fetch error.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("hijacker not supported")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	})
	mux.HandleFunc("/seg_3_102_video_abc.m4s", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("seg-102-data"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pl := &Playlist{
		PlaylistURL: srv.URL + "/playlist.m3u8",
	}

	handler := func(_ []byte, _ float64) error {
		atomic.AddInt32(&handlerCalls, 1)
		return nil
	}

	lastSeq := -1
	initWritten := false
	_, err := pl.processMediaPlaylist(context.Background(), internal.NewReq(), pl.PlaylistURL, handler, nil, &lastSeq, &initWritten)
	if err != nil {
		t.Fatalf("processMediaPlaylist() error = %v", err)
	}

	if got := atomic.LoadInt32(&handlerCalls); got != 2 {
		t.Fatalf("handler called %d times, want 2", got)
	}
	if got := atomic.LoadInt32(&failedRequests); got != 4 {
		t.Fatalf("failed segment requested %d times, want 4 (initial request plus three retries)", got)
	}
	if lastSeq != 102 {
		t.Fatalf("lastSeq = %d, want 102", lastSeq)
	}
}
