package chaturbate

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
)

func TestPairTimelineSegmentsDropsBothSidesOfMissingInterval(t *testing.T) {
	video := []TimedSegment{
		{Start: 0, End: 10},
		{Start: 10, End: 20},
		{Start: 20, End: 30},
	}
	audio := []TimedSegment{
		{Start: 0, End: 10},
		// 10-20 is missing.
		{Start: 20, End: 30},
	}

	pairs, videoPending, audioPending := pairTimelineSegments(video, audio, 30, 30)
	if len(pairs) != 2 {
		t.Fatalf("pairs = %d, want 2", len(pairs))
	}
	if pairs[0].video[0].Start != 0 || pairs[1].video[0].Start != 20 {
		t.Fatalf("paired video intervals = [%v, %v], want [0, 20]", pairs[0].video[0].Start, pairs[1].video[0].Start)
	}
	if len(videoPending) != 0 || len(audioPending) != 0 {
		t.Fatalf("pending video/audio = %d/%d, want 0/0", len(videoPending), len(audioPending))
	}
}

func TestPairTimelineSegmentsSupportsOneVideoToTwoAudio(t *testing.T) {
	video := []TimedSegment{{Start: 0, End: 2}}
	audio := []TimedSegment{{Start: 0, End: 1}, {Start: 1, End: 2}}
	pairs, _, _ := pairTimelineSegments(video, audio, 2, 2)
	if len(pairs) != 1 || len(pairs[0].video) != 1 || len(pairs[0].audio) != 2 {
		t.Fatalf("pair shape = %d video/%d audio", len(pairs[0].video), len(pairs[0].audio))
	}
}

func TestPairTimelineSegmentsAllowsObservedLLHLSTrackSkew(t *testing.T) {
	server.ConfigMu.Lock()
	oldConfig := server.Config
	server.Config = &entity.Config{PairToleranceMS: 1000, PendingSeconds: 30, MaxPendingMB: 512}
	server.ConfigMu.Unlock()
	t.Cleanup(func() {
		server.ConfigMu.Lock()
		server.Config = oldConfig
		server.ConfigMu.Unlock()
	})

	video := []TimedSegment{{Start: 12858.58, End: 12860.18}}
	audio := []TimedSegment{{Start: 12859.20, End: 12860.80}}
	pairs, videoPending, audioPending := pairTimelineSegments(video, audio, 12860.18, 12860.80)
	if len(pairs) != 1 || len(pairs[0].video) != 1 || len(pairs[0].audio) != 1 {
		t.Fatalf("observed 620ms skew pair shape = %d pairs, pending=%d/%d", len(pairs), len(videoPending), len(audioPending))
	}
}

func TestPairTimelineSegmentsLargeToleranceStillSupportsOneToMany(t *testing.T) {
	server.ConfigMu.Lock()
	oldConfig := server.Config
	server.Config = &entity.Config{PairToleranceMS: 1000, PendingSeconds: 30, MaxPendingMB: 512}
	server.ConfigMu.Unlock()
	t.Cleanup(func() {
		server.ConfigMu.Lock()
		server.Config = oldConfig
		server.ConfigMu.Unlock()
	})

	video := []TimedSegment{{Start: 0, End: 2}}
	audio := []TimedSegment{{Start: 0, End: 1}, {Start: 1, End: 2}}
	pairs, _, _ := pairTimelineSegments(video, audio, 2, 2)
	if len(pairs) != 1 || len(pairs[0].video) != 1 || len(pairs[0].audio) != 2 {
		t.Fatalf("pair shape with 1s tolerance = %d pairs, video/audio=%d/%d", len(pairs), len(pairs[0].video), len(pairs[0].audio))
	}
}

func TestPairTimelineSegmentsRetainsTrackUntilOtherTimelineCatchesUp(t *testing.T) {
	video := []TimedSegment{{Start: 20, End: 30}}
	pairs, videoPending, _ := pairTimelineSegments(video, nil, 30, 10)
	if len(pairs) != 0 || len(videoPending) != 1 {
		t.Fatalf("pairs/pending = %d/%d, want 0/1", len(pairs), len(videoPending))
	}
}

func buildTrackParts(t *testing.T, mediaType string, timescale uint32) ([]byte, []byte) {
	t.Helper()
	init := mp4.CreateEmptyInit()
	init.AddEmptyTrack(timescale, mediaType, "und")
	if err := init.TweakSingleTrakLive(); err != nil {
		t.Fatal(err)
	}
	var initBuffer bytes.Buffer
	if err := init.Encode(&initBuffer); err != nil {
		t.Fatal(err)
	}
	segment := mp4.NewMediaSegmentWithoutStyp()
	fragment, err := mp4.CreateFragment(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := fragment.AddFullSampleToTrack(mp4.FullSample{Sample: mp4.Sample{Dur: timescale, Size: 1, Flags: mp4.SyncSampleFlags}, Data: []byte{1}}, 1); err != nil {
		t.Fatal(err)
	}
	segment.AddFragment(fragment)
	var mediaBuffer bytes.Buffer
	if err := segment.Encode(&mediaBuffer); err != nil {
		t.Fatal(err)
	}
	return initBuffer.Bytes(), mediaBuffer.Bytes()
}

func TestWatchPairedSegmentsEndToEnd(t *testing.T) {
	videoInit, videoMedia := buildTrackParts(t, "video", 90000)
	audioInit, audioMedia := buildTrackParts(t, "audio", 48000)
	playlist := func(init, media string) string {
		return strings.Join([]string{"#EXTM3U", "#EXT-X-VERSION:7", "#EXT-X-TARGETDURATION:1", "#EXT-X-MEDIA-SEQUENCE:100", `#EXT-X-MAP:URI="` + init + `"`, "#EXTINF:1.0,", media, ""}, "\n")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/video.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(playlist("/video-init.mp4", "/seg_1_100_video_x.m4s")))
	})
	mux.HandleFunc("/audio.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(playlist("/audio-init.mp4", "/seg_1_100_audio_x.m4s")))
	})
	mux.HandleFunc("/video-init.mp4", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(videoInit) })
	mux.HandleFunc("/audio-init.mp4", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(audioInit) })
	mux.HandleFunc("/seg_1_100_video_x.m4s", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(videoMedia) })
	mux.HandleFunc("/seg_1_100_audio_x.m4s", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(audioMedia) })
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handled := false
	p := &Playlist{PlaylistURL: httpServer.URL + "/video.m3u8", AudioPlaylistURL: httpServer.URL + "/audio.m3u8"}
	err := p.watchPairedSegments(ctx, func(_, _ []byte) error { return nil }, func(video, audio []TimedSegment) error {
		handled = len(video) == 1 && len(audio) == 1
		cancel()
		return nil
	}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("watch error = %v, want context canceled", err)
	}
	if !handled {
		t.Fatal("aligned A/V segments were not emitted")
	}
}

func TestFetchMediaBatchRefreshesWhenAllSegmentsForbidden(t *testing.T) {
	initData, _ := buildTrackParts(t, "video", 90000)
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:1\n#EXT-X-MAP:URI=\"/init.mp4\"\n#EXTINF:1,\nopaque.m4s\n"))
	})
	mux.HandleFunc("/init.mp4", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(initData) })
	mux.HandleFunc("/opaque.m4s", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "expired", http.StatusForbidden) })
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	_, err := fetchMediaBatch(context.Background(), internal.NewReq(), &mediaTrackState{playlistURL: httpServer.URL + "/playlist.m3u8"})
	if !errors.Is(err, internal.ErrPlaylistForbidden) {
		t.Fatalf("error = %v, want ErrPlaylistForbidden", err)
	}
}
