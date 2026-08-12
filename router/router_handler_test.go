package router

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	appconfig "github.com/teacat/chaturbate-dvr/config"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/server"
)

func TestNormalizeUsernamesSupportsBatchInput(t *testing.T) {
	got, skipped, err := normalizeUsernames(" Alice, bob\nCAROL；dave ")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("unexpected skipped usernames: %v", skipped)
	}
	want := []string{"alice", "bob", "carol", "dave"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestNormalizeUsernamesSkipsDuplicate(t *testing.T) {
	got, skipped, err := normalizeUsernames("Alice, alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "alice" || len(skipped) != 1 || skipped[0] != "alice" {
		t.Fatalf("got=%v skipped=%v", got, skipped)
	}
}

func TestSafeRecordingPattern(t *testing.T) {
	for _, pattern := range []string{"videos/model/file", "videos/{{.Username}}/file"} {
		if !safeRecordingPattern(pattern) {
			t.Fatalf("safe pattern rejected: %q", pattern)
		}
	}
	for _, pattern := range []string{"", "../escape", `..\escape`, "videos/../../escape", "videos/model/../file", "/tmp/escape", `C:\\escape`} {
		if safeRecordingPattern(pattern) {
			t.Fatalf("unsafe pattern accepted: %q", pattern)
		}
	}
}

func TestTrustedAbsoluteRecordingPatternStaysWithinConfiguredRoot(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "{{.Username}}", "recording")
	for _, pattern := range []string{
		current,
		filepath.Join(root, "archive", "{{.Username}}", "recording"),
	} {
		if !allowedRecordingPattern(pattern, current) {
			t.Fatalf("trusted absolute pattern rejected: %q", pattern)
		}
	}

	outside := filepath.Join(filepath.Dir(root), "outside", "recording")
	parentReference := root + string(filepath.Separator) + ".." + string(filepath.Separator) + "outside"
	for _, pattern := range []string{outside, parentReference} {
		if allowedRecordingPattern(pattern, current) {
			t.Fatalf("absolute pattern outside trusted root accepted: %q", pattern)
		}
	}
}

func TestTrustedAbsoluteRecordingPatternDoesNotTrustFilesystemRoot(t *testing.T) {
	volumeRoot := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	current := filepath.Join(volumeRoot, "{{.Username}}", "recording")
	candidate := filepath.Join(volumeRoot, "outside", "recording")
	if trustedAbsoluteRecordingPattern(candidate, current) {
		t.Fatalf("filesystem root was accepted as a trusted recording root: %q", current)
	}
}

func TestUpdateConfigAllowsExistingTrustedAbsoluteRoot(t *testing.T) {
	chdirTest(t, t.TempDir())
	recordingRoot := filepath.Join(t.TempDir(), "downloads")
	currentPattern := filepath.Join(recordingRoot, "{{.Username}}", "recording")
	setTestServerConfig(t, &entity.Config{
		Framerate: 30, Resolution: 1080, Pattern: currentPattern,
	})

	response := submitConfigUpdate(currentPattern)
	if response.Code != http.StatusFound {
		t.Fatalf("UpdateConfig status = %d, body = %q", response.Code, response.Body.String())
	}
	settings, err := appconfig.LoadGlobalSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings == nil || settings.Pattern != currentPattern {
		t.Fatalf("saved pattern = %#v, want %q", settings, currentPattern)
	}
	if settings.PairToleranceMS != 1000 {
		t.Fatalf("saved pair tolerance = %d, want 1000", settings.PairToleranceMS)
	}
	server.ConfigMu.RLock()
	gotTolerance := server.Config.PairToleranceMS
	server.ConfigMu.RUnlock()
	if gotTolerance != 1000 {
		t.Fatalf("in-memory pair tolerance = %d, want 1000", gotTolerance)
	}
}

func TestUpdateConfigSaveFailureRollsBackMemory(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "conf"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	original := &entity.Config{Framerate: 30, Resolution: 1080, Pattern: "videos/original", PairToleranceMS: 750}
	setTestServerConfig(t, original)

	response := submitConfigUpdate("videos/changed")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("UpdateConfig status = %d, body = %q", response.Code, response.Body.String())
	}
	server.ConfigMu.RLock()
	got := *server.Config
	server.ConfigMu.RUnlock()
	if got.Pattern != original.Pattern || got.Framerate != original.Framerate || got.Resolution != original.Resolution || got.PairToleranceMS != original.PairToleranceMS {
		t.Fatalf("in-memory config changed after save failure: %#v", got)
	}
}

func TestUpdateConfigRejectsInvalidPairTolerance(t *testing.T) {
	for _, value := range []string{"0", "5001"} {
		setTestServerConfig(t, &entity.Config{Framerate: 30, Resolution: 1080, Pattern: "videos/original", PairToleranceMS: 1000})
		response := submitConfigUpdateWithTolerance("videos/original", value)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("pair tolerance %s status = %d, want 400", value, response.Code)
		}
	}
}

func setTestServerConfig(t *testing.T, conf *entity.Config) {
	t.Helper()
	server.ConfigMu.Lock()
	previous := server.Config
	server.Config = conf
	server.ConfigMu.Unlock()
	t.Cleanup(func() {
		server.ConfigMu.Lock()
		server.Config = previous
		server.ConfigMu.Unlock()
	})
}

func chdirTest(t *testing.T, dir string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
}

func submitConfigUpdate(pattern string) *httptest.ResponseRecorder {
	return submitConfigUpdateWithTolerance(pattern, "1000")
}

func submitConfigUpdateWithTolerance(pattern, pairTolerance string) *httptest.ResponseRecorder {
	form := url.Values{
		"framerate":         {"60"},
		"resolution":        {"720"},
		"pattern":           {pattern},
		"max_duration":      {"120"},
		"max_filesize":      {"4096"},
		"pair_tolerance_ms": {pairTolerance},
	}
	request := httptest.NewRequest(http.MethodPost, "/update_config", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	UpdateConfig(context)
	context.Writer.WriteHeaderNow()
	return response
}
