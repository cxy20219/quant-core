package panel

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

// TestPanelEndpoints 验证面板页面与 JSON 接口(空湖也能正常返回)。
func TestPanelEndpoints(t *testing.T) {
	reg, err := schema.Load(filepath.Join("..", "..", "schemas", "datasets.yaml"))
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	srv := NewServer(Config{Lake: lake.New(t.TempDir(), reg)})
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("panel status=%d", rec.Code)
	}
	html := rec.Body.String()
	for _, needle := range []string{"管理面板", "数据同步", "回测结果", "因子(规划中)", "/panel/api/sync"} {
		if !strings.Contains(html, needle) {
			t.Errorf("页面缺少 %q", needle)
		}
	}

	for _, path := range []string{"/panel/api/overview", "/panel/api/sync", "/panel/api/sources"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s invalid json: %v", path, err)
		}
	}

	// 数据集列表应覆盖注册表全部数据集(空湖时 missing=true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/sync", nil))
	var sync struct {
		Datasets []struct {
			Dataset string `json:"dataset"`
			Missing bool   `json:"missing"`
		} `json:"datasets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sync); err != nil {
		t.Fatal(err)
	}
	if len(sync.Datasets) != len(reg.Datasets) {
		t.Errorf("数据集数=%d, 期望 %d", len(sync.Datasets), len(reg.Datasets))
	}
	for _, row := range sync.Datasets {
		if !row.Missing {
			t.Errorf("空湖中 %s 应为 missing", row.Dataset)
		}
	}
}
