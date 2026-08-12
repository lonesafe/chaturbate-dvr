package view

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
)

//go:embed templates
var FS embed.FS

// InfoTpl is a template for rendering channel information.
var InfoTpl *template.Template

func init() {
	var err error

	InfoTpl, err = template.New("update").ParseFS(FS, "templates/channel_info.html")
	if err != nil {
		log.Fatalf("解析模板失败：%v", err)
	}
}

// StaticFS initializes the static file system for serving frontend files.
func StaticFS() (http.FileSystem, error) {
	frontendFS, err := fs.Sub(FS, "templates")
	if err != nil {
		return nil, fmt.Errorf("初始化静态文件失败：%w", err)
	}
	return http.FS(frontendFS), nil
}
