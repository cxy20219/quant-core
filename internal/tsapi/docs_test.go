package tsapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"quant-core/internal/lake"
	"quant-core/internal/schema"
)

// TestDocsAndOpenAPI 验证 /docs 页面与 OpenAPI 规范由接口表+注册表生成。
func TestDocsAndOpenAPI(t *testing.T) {
	reg, err := schema.Load(filepath.Join("..", "..", "schemas", "datasets.yaml"))
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	srv := NewServer(lake.New(t.TempDir(), reg))
	srv.Logf = t.Logf

	rec := httptest.NewRecorder()
	srv.DocsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("docs status=%d", rec.Code)
	}
	html := rec.Body.String()
	for _, needle := range []string{"stk_mins", "bars_1m", "tushare 兼容数据接口", "回测接口", "策略 API", "单位换算"} {
		if !strings.Contains(html, needle) {
			t.Errorf("docs 缺少 %q", needle)
		}
	}
	// 接口数量与接口表一致
	if got := strings.Count(html, `class="card"`); got < len(DefaultAPIs()) {
		t.Errorf("docs 卡片数=%d, 期望 >=%d", got, len(DefaultAPIs()))
	}

	rec = httptest.NewRecorder()
	srv.OpenAPIHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/openapi.json", nil))
	var spec map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("openapi json: %v", err)
	}
	if spec["openapi"] != "3.0.3" {
		t.Fatalf("openapi version=%v", spec["openapi"])
	}
	paths, _ := spec["paths"].(map[string]any)
	for _, want := range []string{"/", "/api/backtests", "/api/backtests/{id}", "/healthz"} {
		if _, ok := paths[want]; !ok {
			t.Errorf("openapi 缺少路径 %s", want)
		}
	}
}
