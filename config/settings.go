package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/teacat/chaturbate-dvr/entity"
)

type GlobalSettings struct {
	Cookies         string `json:"cookies"`
	UserAgent       string `json:"user_agent"`
	Framerate       int    `json:"framerate"`
	Resolution      int    `json:"resolution"`
	Pattern         string `json:"pattern"`
	MaxDuration     int    `json:"max_duration"`
	MaxFilesize     int    `json:"max_filesize"`
	Compress        bool   `json:"compress"`
	PairToleranceMS int    `json:"pair_tolerance_ms"`
}

func LoadGlobalSettings() (*GlobalSettings, error) {
	data, err := os.ReadFile("./conf/settings.json")
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取全局设置：%w", err)
	}
	var settings GlobalSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("解析全局设置：%w", err)
	}
	return &settings, nil
}

func SaveGlobalSettings(conf *entity.Config) error {
	data, err := json.MarshalIndent(GlobalSettings{
		Cookies: conf.Cookies, UserAgent: conf.UserAgent,
		Framerate: conf.Framerate, Resolution: conf.Resolution, Pattern: conf.Pattern,
		MaxDuration: conf.MaxDuration, MaxFilesize: conf.MaxFilesize, Compress: conf.Compress,
		PairToleranceMS: conf.PairToleranceMS,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll("./conf", 0755); err != nil {
		return err
	}
	temp, err := os.CreateTemp("./conf", "settings-*.tmp")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return replaceSettingsFile(name, "./conf/settings.json")
}
