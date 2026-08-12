package config

import (
	"os"
	"testing"

	"github.com/teacat/chaturbate-dvr/entity"
)

func TestGlobalSettingsRoundTrip(t *testing.T) {
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	want := &entity.Config{
		Cookies: "sessionid=secret", UserAgent: "agent", Framerate: 60, Resolution: 1080,
		Pattern: "videos/{{.Username}}", MaxDuration: 30, MaxFilesize: 1024, Compress: true,
		PairToleranceMS: 1000,
	}
	if err := SaveGlobalSettings(want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadGlobalSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.Cookies != want.Cookies || got.UserAgent != want.UserAgent || got.Framerate != want.Framerate ||
		got.Resolution != want.Resolution || got.Pattern != want.Pattern || got.MaxDuration != want.MaxDuration ||
		got.MaxFilesize != want.MaxFilesize || got.Compress != want.Compress || got.PairToleranceMS != want.PairToleranceMS {
		t.Fatalf("settings = %#v", got)
	}
	info, err := os.Stat("conf/settings.json")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatalf("settings permissions = %o, want private", info.Mode().Perm())
	}
}
