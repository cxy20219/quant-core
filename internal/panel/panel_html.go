package panel

import (
	_ "embed"
	"html/template"
)

// pageHTML 是管理面板页面(独立文件,go:embed 嵌入;自包含样式与脚本,无 CDN)。
//
//go:embed page.html
var pageHTML string

var pageTemplate = template.Must(template.New("panel").Parse(pageHTML))
