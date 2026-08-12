package chaturbate

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Eyevinn/mp4ff/bits"
	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/avast/retry-go/v4"
	"github.com/grafov/m3u8"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
)

const segmentDownloadWorkers = 6

const (
	pairingToleranceSeconds = 1.0
	maxPendingSeconds       = 60.0
	maxPendingSegments      = 240
)

func pairingTolerance() float64 {
	server.ConfigMu.RLock()
	defer server.ConfigMu.RUnlock()
	if server.Config != nil && server.Config.PairToleranceMS > 0 {
		return float64(server.Config.PairToleranceMS) / 1000
	}
	return pairingToleranceSeconds
}

func pendingWindowSeconds() float64 {
	if server.Config != nil && server.Config.PendingSeconds > 0 {
		return float64(server.Config.PendingSeconds)
	}
	return maxPendingSeconds
}

func workerLimit() int {
	if server.Config != nil && server.Config.SegmentWorkers > 0 {
		return server.Config.SegmentWorkers
	}
	return segmentDownloadWorkers
}

// TimedSegment is a downloaded fMP4 segment positioned on its media timeline.
type TimedSegment struct {
	Data     []byte
	Start    float64
	End      float64
	Duration float64
}

type mediaTrackState struct {
	playlistURL string
	initData    []byte
	initWritten bool
	lastSeq     uint64
	hasLastSeq  bool
	pending     []TimedSegment
	maxSeenEnd  float64
	mapURI      string
	workers     int
	pollCount   uint64
	name        string
}

type mediaBatch struct {
	segments         []TimedSegment
	pollInterval     time.Duration
	playlistSeq      uint64
	playlistSegments int
	newSegments      int
	downloaded       int
	failures         int
	workers          int
	hasRange         bool
	firstStart       float64
	lastEnd          float64
}

type AVInitHandler func(videoInit, audioInit []byte) error
type AVSegmentHandler func(video, audio []TimedSegment) error

type timedSegmentPair struct {
	video []TimedSegment
	audio []TimedSegment
}

// watchPairedSegments downloads both tracks concurrently and only emits A/V
// segments whose tfdt timelines overlap. Unmatched segments are retained until
// the opposite track has advanced beyond them, at which point they are known
// to be missing and are discarded.
func (p *Playlist) watchPairedSegments(ctx context.Context, initHandler AVInitHandler, segmentHandler AVSegmentHandler, pollComplete PollCompleteHandler) error {
	video := &mediaTrackState{name: "video", playlistURL: p.PlaylistURL}
	audio := &mediaTrackState{name: "audio", playlistURL: p.AudioPlaylistURL}
	client := p.client
	if client == nil {
		client = internal.NewReq()
	}

	noPairPolls := 0
	for {
		var videoBatch, audioBatch mediaBatch
		var videoErr, audioErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			videoBatch, videoErr = fetchMediaBatch(ctx, client, video)
		}()
		go func() {
			defer wg.Done()
			audioBatch, audioErr = fetchMediaBatch(ctx, client, audio)
		}()
		wg.Wait()
		if videoErr != nil {
			return fmt.Errorf("视频轨：%w", videoErr)
		}
		if audioErr != nil {
			return fmt.Errorf("音频轨：%w", audioErr)
		}

		if !video.initWritten || !audio.initWritten {
			if err := initHandler(video.initData, audio.initData); err != nil {
				return fmt.Errorf("初始化实时音视频合并：%w", err)
			}
			video.initWritten = true
			audio.initWritten = true
		}

		video.pending = append(video.pending, videoBatch.segments...)
		audio.pending = append(audio.pending, audioBatch.segments...)
		beforeVideoPending := len(video.pending)
		beforeAudioPending := len(audio.pending)
		pairs, videoPending, audioPending := pairTimelineSegments(video.pending, audio.pending, video.maxSeenEnd, audio.maxSeenEnd)
		video.pending, audio.pending = videoPending, audioPending
		if len(pairs) == 0 && (beforeVideoPending > 0 || beforeAudioPending > 0 || videoBatch.newSegments > 0 || audioBatch.newSegments > 0) {
			noPairPolls++
			if noPairPolls == 1 || noPairPolls%5 == 0 {
				log.Printf("[诊断] 双轨播放列表连续 %d 次轮询没有可配对的音视频分片；容差=%.3f 秒；视频={%s 配对前待处理=%d 配对后待处理=%s 水位=%.3f}；音频={%s 配对前待处理=%d 配对后待处理=%s 水位=%.3f}",
					noPairPolls,
					pairingTolerance(),
					videoBatch.diagnosticSummary(), beforeVideoPending, segmentRangeSummary(video.pending), video.maxSeenEnd,
					audioBatch.diagnosticSummary(), beforeAudioPending, segmentRangeSummary(audio.pending), audio.maxSeenEnd,
				)
			}
		} else if len(pairs) > 0 {
			if noPairPolls > 0 {
				log.Printf("[诊断] 双轨播放列表在连续 %d 次无配对后恢复；本次配对=%d；视频待处理=%s；音频待处理=%s",
					noPairPolls, len(pairs), segmentRangeSummary(video.pending), segmentRangeSummary(audio.pending))
			}
			noPairPolls = 0
		}
		for _, pair := range pairs {
			if err := segmentHandler(pair.video, pair.audio); err != nil {
				return fmt.Errorf("处理已配对分片：%w", err)
			}
		}

		if pollComplete != nil {
			if err := pollComplete(); err != nil {
				return fmt.Errorf("完成播放列表轮询：%w", err)
			}
		}
		pollInterval := pickPollInterval(videoBatch.pollInterval, audioBatch.pollInterval)
		if pollInterval < 2*time.Second {
			pollInterval = 2 * time.Second
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func fetchMediaBatch(ctx context.Context, client *internal.Req, state *mediaTrackState) (mediaBatch, error) {
	startedAt := time.Now()
	body, err := client.Get(ctx, state.playlistURL)
	if err != nil {
		if internal.IsHTTPStatus(err, http.StatusForbidden, http.StatusNotFound) {
			return mediaBatch{}, fmt.Errorf("%w: %v", internal.ErrPlaylistForbidden, err)
		}
		return mediaBatch{}, fmt.Errorf("获取播放列表：%w", err)
	}
	decoded, _, err := m3u8.DecodeFrom(bytes.NewReader([]byte(body)), true)
	if err != nil {
		return mediaBatch{}, fmt.Errorf("解析播放列表：%w", err)
	}
	pl, ok := decoded.(*m3u8.MediaPlaylist)
	if !ok {
		return mediaBatch{}, fmt.Errorf("预期为媒体播放列表")
	}
	batch := mediaBatch{pollInterval: time.Duration(pl.TargetDuration) * time.Second, playlistSeq: pl.SeqNo}
	for _, segment := range pl.Segments {
		if segment != nil {
			batch.playlistSegments++
		}
	}

	state.pollCount++
	if pl.Map != nil && pl.Map.URI != "" && state.mapURI != "" && state.mapURI != pl.Map.URI {
		return mediaBatch{}, fmt.Errorf("%w：%s 轨初始化 URI 在播放列表序号 %d 处变化", internal.ErrTimelineReset, trackNameText(state.name), pl.SeqNo)
	}
	if pl.Map != nil && pl.Map.URI != "" && (len(state.initData) == 0 || state.pollCount%30 == 0) {
		state.mapURI = pl.Map.URI
		latestInit, initErr := downloadSegment(ctx, client, resolveURL(state.playlistURL, pl.Map.URI))
		if internal.IsHTTPStatus(initErr, http.StatusForbidden, http.StatusNotFound) {
			return mediaBatch{}, fmt.Errorf("%w：初始化分片不可用", internal.ErrPlaylistForbidden)
		}
		err = initErr
		if err != nil {
			return mediaBatch{}, fmt.Errorf("下载初始化分片：%w", err)
		}
		if len(state.initData) > 0 && !bytes.Equal(state.initData, latestInit) {
			return mediaBatch{}, fmt.Errorf("%w：%s 轨初始化分片内容在播放列表序号 %d 处变化", internal.ErrTimelineReset, trackNameText(state.name), pl.SeqNo)
		}
		state.initData = latestInit
	}

	type job struct {
		index    int
		seq      uint64
		url      string
		duration float64
	}
	jobs := make([]job, 0, len(pl.Segments))
	for index, segment := range pl.Segments {
		if segment == nil {
			continue
		}
		seq := pl.SeqNo + uint64(index)
		if state.hasLastSeq && seq <= state.lastSeq {
			continue
		}
		if segment.Discontinuity {
			return mediaBatch{}, fmt.Errorf("%w：%s 轨在媒体序号 %d 处出现不连续标记", internal.ErrTimelineReset, trackNameText(state.name), seq)
		}
		jobs = append(jobs, job{index: len(jobs), seq: seq, url: resolveURL(state.playlistURL, segment.URI), duration: segment.Duration})
	}
	batch.newSegments = len(jobs)
	if len(jobs) == 0 {
		return batch, nil
	}

	type result struct {
		index   int
		seq     uint64
		segment TimedSegment
		err     error
	}
	jobCh := make(chan job)
	resultCh := make(chan result, len(jobs))
	workers := state.workers
	limit := workerLimit()
	if workers < 2 {
		workers = 2
	}
	if workers > limit {
		workers = limit
	}
	if len(jobs) < workers {
		workers = len(jobs)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for current := range jobCh {
				data, downloadErr := downloadSegment(ctx, client, current.url)
				if downloadErr != nil {
					resultCh <- result{index: current.index, seq: current.seq, err: downloadErr}
					continue
				}
				start, end, timingErr := fragmentTimeline(state.initData, data)
				resultCh <- result{index: current.index, seq: current.seq, segment: TimedSegment{Data: data, Start: start, End: end, Duration: current.duration}, err: timingErr}
			}
		}()
	}
	go func() {
		for _, current := range jobs {
			jobCh <- current
		}
		close(jobCh)
		wg.Wait()
		close(resultCh)
	}()

	results := make([]result, 0, len(jobs))
	for downloaded := range resultCh {
		results = append(results, downloaded)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].index < results[j].index })
	failures := 0
	for _, downloaded := range results {
		if downloaded.seq > state.lastSeq {
			state.lastSeq = downloaded.seq
			state.hasLastSeq = true
		}
		if downloaded.err != nil {
			failures++
			continue
		}
		if state.maxSeenEnd > 0 && downloaded.segment.Start < state.maxSeenEnd-pairingTolerance() {
			return mediaBatch{}, fmt.Errorf("%w：%s 轨 tfdt 在媒体序号 %d 处回退，开始=%.3f，上次结束=%.3f，容差=%.3f",
				internal.ErrTimelineReset, trackNameText(state.name), downloaded.seq, downloaded.segment.Start, state.maxSeenEnd, pairingTolerance())
		}
		batch.segments = append(batch.segments, downloaded.segment)
		if !batch.hasRange || downloaded.segment.Start < batch.firstStart {
			batch.firstStart = downloaded.segment.Start
		}
		if downloaded.segment.End > batch.lastEnd {
			batch.lastEnd = downloaded.segment.End
		}
		batch.hasRange = true
		if downloaded.segment.End > state.maxSeenEnd {
			state.maxSeenEnd = downloaded.segment.End
		}
	}
	batch.downloaded = len(batch.segments)
	batch.failures = failures
	batch.workers = workers
	if len(results) > 0 && failures == len(results) {
		allForbidden := true
		for _, downloaded := range results {
			if !internal.IsHTTPStatus(downloaded.err, http.StatusForbidden) {
				allForbidden = false
				break
			}
		}
		if allForbidden {
			return mediaBatch{}, fmt.Errorf("%w：%s 轨在播放列表序号 %d 的全部媒体分片均返回 403", internal.ErrPlaylistForbidden, trackNameText(state.name), pl.SeqNo)
		}
	}
	if failures > len(results)/3 && workers > 2 {
		state.workers = workers - 1
	} else if len(jobs) > workers*2 && time.Since(startedAt) < batch.pollInterval && workers < limit {
		state.workers = workers + 1
	} else {
		state.workers = workers
	}
	return batch, nil
}

func (batch mediaBatch) diagnosticSummary() string {
	timeRange := "时间范围=无"
	if batch.hasRange {
		timeRange = fmt.Sprintf("时间范围=%.3f-%.3f", batch.firstStart, batch.lastEnd)
	}
	return fmt.Sprintf("序号=%d 列表分片=%d 新分片=%d 成功=%d 失败=%d 下载线程=%d 目标时长=%s %s",
		batch.playlistSeq,
		batch.playlistSegments,
		batch.newSegments,
		batch.downloaded,
		batch.failures,
		batch.workers,
		batch.pollInterval,
		timeRange,
	)
}

func segmentRangeSummary(segments []TimedSegment) string {
	if len(segments) == 0 {
		return "0"
	}
	firstStart, lastEnd := segments[0].Start, segments[0].End
	for _, segment := range segments[1:] {
		if segment.Start < firstStart {
			firstStart = segment.Start
		}
		if segment.End > lastEnd {
			lastEnd = segment.End
		}
	}
	return fmt.Sprintf("%d 个，时间范围=%.3f-%.3f", len(segments), firstStart, lastEnd)
}

func trackNameText(name string) string {
	switch name {
	case "video":
		return "视频"
	case "audio":
		return "音频"
	default:
		return name
	}
}

func downloadSegment(ctx context.Context, client *internal.Req, segmentURL string) ([]byte, error) {
	return retry.DoWithData(func() ([]byte, error) {
		return client.GetBytes(ctx, segmentURL)
	}, retry.Context(ctx), retry.Attempts(3), retry.Delay(600*time.Millisecond), retry.DelayType(func(attempt uint, err error, _ *retry.Config) time.Duration {
		if delay := internal.HTTPRetryAfter(err); delay > 0 {
			return delay
		}
		delay := 600 * time.Millisecond * time.Duration(1<<min(attempt, 4))
		return delay + time.Duration(time.Now().UnixNano()%int64(250*time.Millisecond))
	}), retry.RetryIf(func(err error) bool {
		return !internal.IsHTTPStatus(err, http.StatusForbidden, http.StatusNotFound)
	}))
}

func fragmentTimeline(initData, segmentData []byte) (float64, float64, error) {
	initFile, err := mp4.DecodeFileSR(bits.NewFixedSliceReader(initData))
	if err != nil || initFile.Init == nil || len(initFile.Init.Moov.Traks) != 1 {
		return 0, 0, fmt.Errorf("解析初始化分片：%w", err)
	}
	trak := initFile.Init.Moov.Traks[0]
	if trak.Mdia == nil || trak.Mdia.Mdhd == nil || trak.Mdia.Mdhd.Timescale == 0 {
		return 0, 0, fmt.Errorf("缺少媒体时间刻度")
	}
	if initFile.Init.Moov.Mvex == nil {
		return 0, 0, fmt.Errorf("缺少 mvex")
	}
	trex, ok := initFile.Init.Moov.Mvex.GetTrex(trak.Tkhd.TrackID)
	if !ok {
		return 0, 0, fmt.Errorf("缺少 trex")
	}
	mediaFile, err := mp4.DecodeFileSR(bits.NewFixedSliceReader(segmentData))
	if err != nil {
		return 0, 0, fmt.Errorf("解析媒体分片：%w", err)
	}
	var first, end uint64
	found := false
	for _, mediaSegment := range mediaFile.Segments {
		for _, fragment := range mediaSegment.Fragments {
			samples, sampleErr := fragment.GetFullSamples(trex)
			if sampleErr != nil {
				return 0, 0, sampleErr
			}
			for _, sample := range samples {
				if !found || sample.DecodeTime < first {
					first = sample.DecodeTime
				}
				sampleEnd := sample.DecodeTime + uint64(sample.Dur)
				if sampleEnd > end {
					end = sampleEnd
				}
				found = true
			}
		}
	}
	if !found {
		return 0, 0, fmt.Errorf("媒体分片中没有采样数据")
	}
	timescale := float64(trak.Mdia.Mdhd.Timescale)
	return float64(first) / timescale, float64(end) / timescale, nil
}

func pairTimelineSegments(video, audio []TimedSegment, videoWatermark, audioWatermark float64) ([]timedSegmentPair, []TimedSegment, []TimedSegment) {
	tolerance := pairingTolerance()
	sort.Slice(video, func(i, j int) bool { return video[i].Start < video[j].Start })
	sort.Slice(audio, func(i, j int) bool { return audio[i].Start < audio[j].Start })
	usedAudio := make([]bool, len(audio))
	pairedVideo := make([]bool, len(video))
	pairs := make([]timedSegmentPair, 0)
	for vi, ai := 0, 0; vi < len(video) && ai < len(audio); {
		// These checks answer only whether the intervals overlap. Applying the
		// boundary tolerance here used to make genuinely overlapping segments
		// look disjoint and also consumed the wrong side of missing intervals.
		if video[vi].End <= audio[ai].Start {
			vi++
			continue
		}
		if audio[ai].End <= video[vi].Start {
			ai++
			continue
		}
		vStart, aStart := vi, ai
		vEnd, aEnd := video[vi].End, audio[ai].End
		for {
			currentDiff := absFloat(vEnd - aEnd)
			if vEnd < aEnd {
				if vi+1 >= len(video) || video[vi+1].Start > vEnd+tolerance {
					break
				}
				nextEnd := video[vi+1].End
				// Even when the current boundary is within tolerance, include a
				// contiguous fragment if it aligns the group more closely. This
				// preserves one-to-many matching with larger real-world skew.
				if currentDiff <= tolerance && absFloat(nextEnd-aEnd) >= currentDiff {
					break
				}
				vi++
				vEnd = nextEnd
			} else if aEnd < vEnd {
				if ai+1 >= len(audio) || audio[ai+1].Start > aEnd+tolerance {
					break
				}
				nextEnd := audio[ai+1].End
				if currentDiff <= tolerance && absFloat(vEnd-nextEnd) >= currentDiff {
					break
				}
				ai++
				aEnd = nextEnd
			} else {
				break
			}
		}
		if absFloat(video[vStart].Start-audio[aStart].Start) <= tolerance && absFloat(vEnd-aEnd) <= tolerance {
			for i := vStart; i <= vi; i++ {
				pairedVideo[i] = true
			}
			for i := aStart; i <= ai; i++ {
				usedAudio[i] = true
			}
			pairs = append(pairs, timedSegmentPair{video: append([]TimedSegment(nil), video[vStart:vi+1]...), audio: append([]TimedSegment(nil), audio[aStart:ai+1]...)})
		}
		vi++
		ai++
	}
	videoPending := make([]TimedSegment, 0)
	for i, segment := range video {
		if !pairedVideo[i] && segment.End > audioWatermark {
			videoPending = append(videoPending, segment)
		}
	}
	audioPending := make([]TimedSegment, 0)
	for i, segment := range audio {
		if !usedAudio[i] && segment.End > videoWatermark {
			audioPending = append(audioPending, segment)
		}
	}
	return pairs, trimPending(videoPending, videoWatermark), trimPending(audioPending, audioWatermark)
}

func trimPending(segments []TimedSegment, watermark float64) []TimedSegment {
	cutoff := watermark - pendingWindowSeconds()
	kept := segments[:0]
	for _, segment := range segments {
		if segment.End > cutoff {
			kept = append(kept, segment)
		}
	}
	limit := maxPendingSegments
	if server.Config != nil && server.Config.PendingSeconds > 0 {
		limit = max(server.Config.PendingSeconds*4, 16)
	}
	if len(kept) > limit {
		kept = kept[len(kept)-limit:]
	}
	maxBytes := 512 << 20
	if server.Config != nil && server.Config.MaxPendingMB > 0 {
		maxBytes = server.Config.MaxPendingMB << 20
	}
	total := 0
	start := len(kept)
	for start > 0 {
		size := len(kept[start-1].Data)
		if total+size > maxBytes {
			break
		}
		total += size
		start--
	}
	kept = kept[start:]
	return kept
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
