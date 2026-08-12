package channel

import "testing"

func TestRoomStatusText(t *testing.T) {
	tests := map[string]string{
		"public":             "公开直播",
		"private":            "私密直播",
		"away":               "暂时离开",
		"offline":            "离线",
		"group":              "群组直播",
		"hidden":             "隐藏",
		"password protected": "密码保护",
		"":                   "未知",
		"custom":             "custom",
	}
	for status, want := range tests {
		if got := roomStatusText(status); got != want {
			t.Errorf("roomStatusText(%q) = %q, want %q", status, got, want)
		}
	}
}
