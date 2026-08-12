package config

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/urfave/cli/v2"
)

func TestNewDoesNotAutoEnableCompressWhenFFmpegExists(t *testing.T) {
	set := newConfigFlagSet(t)
	t.Setenv("PATH", fakeFFmpegDir(t))

	conf, err := New(newConfigContext(set))
	if err != nil {
		t.Fatalf("new config: %v", err)
	}

	if conf.Compress {
		t.Fatal("compress = true, want false unless --compress is explicitly set")
	}
}

func TestNewEnablesCompressWhenFlagIsExplicit(t *testing.T) {
	set := newConfigFlagSet(t, "--compress")
	t.Setenv("PATH", fakeFFmpegDir(t))

	conf, err := New(newConfigContext(set))
	if err != nil {
		t.Fatalf("new config: %v", err)
	}

	if !conf.Compress {
		t.Fatal("compress = false, want true when --compress is explicitly set")
	}
}

func TestNewLoadsPersistedPairTolerance(t *testing.T) {
	chdirConfigTest(t, t.TempDir())
	if err := os.MkdirAll("conf", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("conf/settings.json", []byte(`{"pair_tolerance_ms":750}`), 0600); err != nil {
		t.Fatal(err)
	}

	conf, err := New(newConfigContext(newConfigFlagSet(t)))
	if err != nil {
		t.Fatalf("new config: %v", err)
	}
	if conf.PairToleranceMS != 750 {
		t.Fatalf("PairToleranceMS = %d, want persisted 750", conf.PairToleranceMS)
	}
}

func TestNewUsesDefaultPairToleranceWithoutPersistedSetting(t *testing.T) {
	chdirConfigTest(t, t.TempDir())

	conf, err := New(newConfigContext(newConfigFlagSet(t)))
	if err != nil {
		t.Fatalf("new config: %v", err)
	}
	if conf.PairToleranceMS != defaultPairToleranceMS {
		t.Fatalf("PairToleranceMS = %d, want default %d", conf.PairToleranceMS, defaultPairToleranceMS)
	}
}

func newConfigFlagSet(t *testing.T, args ...string) *flag.FlagSet {
	t.Helper()

	set := flag.NewFlagSet("test", flag.ContinueOnError)
	compressFlag := &cli.BoolFlag{Name: "compress"}
	if err := compressFlag.Apply(set); err != nil {
		t.Fatalf("apply compress flag: %v", err)
	}
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return set
}

func chdirConfigTest(t *testing.T, dir string) {
	t.Helper()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
}

func newConfigContext(set *flag.FlagSet) *cli.Context {
	return cli.NewContext(&cli.App{Version: "test"}, set, nil)
}

func fakeFFmpegDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return dir
}
