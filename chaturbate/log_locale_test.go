package chaturbate

import (
	"strings"
	"testing"
	"time"
)

func TestMediaBatchDiagnosticSummaryUsesChineseLabels(t *testing.T) {
	summary := (mediaBatch{
		playlistSeq:      7,
		playlistSegments: 4,
		newSegments:      3,
		downloaded:       2,
		failures:         1,
		workers:          2,
		pollInterval:     2 * time.Second,
		hasRange:         true,
		firstStart:       1.5,
		lastEnd:          3.5,
	}).diagnosticSummary()

	for _, want := range []string{"序号=7", "列表分片=4", "成功=2", "失败=1", "下载线程=2", "时间范围=1.500-3.500"} {
		if !strings.Contains(summary, want) {
			t.Errorf("diagnostic summary %q missing %q", summary, want)
		}
	}
}

func TestTrackNameText(t *testing.T) {
	if got := trackNameText("video"); got != "视频" {
		t.Fatalf("trackNameText(video) = %q", got)
	}
	if got := trackNameText("audio"); got != "音频" {
		t.Fatalf("trackNameText(audio) = %q", got)
	}
}
