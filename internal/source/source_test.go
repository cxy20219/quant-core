package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mockRelayB 模拟 B 站风格:下划线接口名、/path 即接口、顶层 count、meta.probe、
// limit 过大返回 503 upstream_pool_exhausted。
func mockRelayB(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
			return
		}
		api := strings.TrimPrefix(r.URL.Path, "/pro/")
		if strings.Contains(api, "-") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_api_name","message":"hyphen not allowed"}`))
			return
		}
		limit := r.URL.Query().Get("limit")
		if limit == "10000" || limit == "5000" {
			// 模拟上游池耗尽
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"ok":false,"error":"upstream_pool_exhausted","message":"limit too large"}`))
			return
		}
		if r.URL.Query().Get("ts_code") == "" && r.URL.Query().Get("exchange") == "" &&
			r.URL.Query().Get("start_date") == "" && r.URL.Query().Get("trade_date") == "" {
			// 无内容参数 → 5 行样例
			items := [][]any{{"x"}, {"x"}, {"x"}, {"x"}, {"x"}}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "data": map[string]any{"fields": []string{"f"}, "items": items},
				"meta": map[string]any{"probe": true, "sample": true},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"fields": []string{"exchange", "cal_date"},
				"items":  [][]any{{"SSE", "20240102"}},
			},
			"count": 1,
			"meta":  map[string]any{"probe": false},
		})
	}))
}

// mockRelayA 模拟 A 站风格:连字符接口名、data 内 count/has_more、缺参 400 报错。
func mockRelayA(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"msg":"missing X-API-Key"}`))
			return
		}
		api := strings.TrimPrefix(r.URL.Path, "/tushare/")
		if !strings.Contains(api, "-") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":400,"msg":"unknown api_name: ` + api + `"}`))
			return
		}
		if r.URL.Query().Get("ts_code") == "" && r.URL.Query().Get("exchange") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":50101,"msg":"必填参数, ts_code"}`))
			return
		}
		if l := r.URL.Query().Get("limit"); l == "5000" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":50101,"msg":"query limit is too large for this high-cardinality interface"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"fields":   []string{"exchange", "cal_date"},
				"items":    [][]any{{"SSE", "20240102"}},
				"count":    1,
				"has_more": false,
			},
		})
	}))
}

func newTestSource(t *testing.T, url string, cfg map[string]any) Source {
	t.Helper()
	merged := map[string]any{"base_url": url}
	for k, v := range cfg {
		merged[k] = v
	}
	src, err := NewTushareHTTP("test", NewConfig(merged, "sources.test"))
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	return src
}

func TestTushareHTTPRelayB(t *testing.T) {
	srv := mockRelayB(t)
	defer srv.Close()
	src := newTestSource(t, srv.URL+"/pro", map[string]any{
		"api_naming": "underscore",
		"auth":       map[string]any{"style": "header", "header": "X-API-Key", "key": "test-key"},
		"limit":      map[string]any{"default": 10000, "max": 10000, "halve_on_reject": true},
		"retry":      map[string]any{"max": 8, "backoff_ms": 10},
		"probe":      map[string]any{"detect": true},
	})

	// 限额被拒 → 自动减半到 2500 成功
	result, err := src.Call(context.Background(), "trade_cal", map[string]any{"exchange": "SSE"}, "")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(result.Items) != 1 || result.Count != 1 {
		t.Fatalf("result = %+v", result)
	}

	// 无内容参数 → probe 样例必须报错
	if _, err := src.Call(context.Background(), "trade_cal", nil, ""); err == nil {
		t.Fatal("expected probe sample to be rejected")
	} else if !strings.Contains(err.Error(), "probe") {
		t.Fatalf("error = %v", err)
	}

	// 鉴权失败必须报错
	srvBad := newTestSource(t, srv.URL+"/pro", map[string]any{
		"api_naming": "underscore",
		"auth":       map[string]any{"style": "header", "header": "X-API-Key", "key": "wrong"},
		"limit":      map[string]any{"default": 1000, "halve_on_reject": true},
	})
	if _, err := srvBad.Call(context.Background(), "trade_cal", map[string]any{"exchange": "SSE"}, ""); err == nil {
		t.Fatal("expected auth error")
	}
}

func TestTushareHTTPRelayA(t *testing.T) {
	srv := mockRelayA(t)
	defer srv.Close()
	src := newTestSource(t, srv.URL+"/tushare", map[string]any{
		"api_naming": "hyphen",
		"auth":       map[string]any{"style": "header", "header": "X-API-Key", "key": "test-key"},
		"limit":      map[string]any{"default": 5000, "max": 5000, "halve_on_reject": true},
	})

	// 连字符转换 + 限额降级(5000 被拒 → 2500 成功)
	result, err := src.Call(context.Background(), "trade_cal", map[string]any{"exchange": "SSE"}, "")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if result.Count != 1 || result.HasMore {
		t.Fatalf("count/has_more 解析错误: %+v", result)
	}

	// 缺参业务错误:不应触发降级重试,直接返回业务错误
	if _, err := src.Call(context.Background(), "trade_cal", nil, ""); err == nil {
		t.Fatal("expected missing-param error")
	} else if !strings.Contains(err.Error(), "必填参数") {
		t.Fatalf("error = %v", err)
	}
}

func TestTushareHTTPOfficialPost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["token"] != "secret" || body["api_name"] != "daily" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":-2001,"msg":"token check fail"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"fields": []string{"ts_code"}, "items": [][]any{{"600000.SH"}}, "has_more": false},
		})
	}))
	defer srv.Close()

	src := newTestSource(t, srv.URL, map[string]any{
		"method":     "POST",
		"auth":       map[string]any{"style": "token_body", "key": "secret"},
		"api_naming": "underscore",
	})
	result, err := src.Call(context.Background(), "daily", map[string]any{"ts_code": "600000.SH"}, "ts_code")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestExecPlugin(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "plugin.py")
	// 一个最小 Python 插件:接收 stdin JSON,回传固定数据
	body := `import json, sys
req = json.load(sys.stdin)
if req["action"] == "check":
    print(json.dumps({"ok": True, "note": "smoke ok"}))
else:
    print(json.dumps({"fields": ["ts_code", "close"], "items": [["600000.SH", 7.31]], "count": 1}))
`
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := NewExec("my-plugin", NewConfig(map[string]any{
		"command": []any{"python", script},
	}, "sources.my-plugin"))
	if err != nil {
		t.Fatalf("new exec: %v", err)
	}
	result, err := src.Call(context.Background(), "daily", map[string]any{"ts_code": "600000.SH"}, "ts_code,close")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0][1].(float64) != 7.31 {
		t.Fatalf("result = %+v", result)
	}
	execSrc, ok := src.(*Exec)
	if !ok {
		t.Fatal("not exec source")
	}
	note, err := execSrc.Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if note != "smoke ok" {
		t.Fatalf("note = %q", note)
	}
}

func TestRegistryLoadAndResolve(t *testing.T) {
	dir := t.TempDir()
	srv := mockRelayB(t)
	defer srv.Close()
	cfg := fmt.Sprintf(`
version: 1
sources:
  relay-b:
    kind: tushare-http
    description: mock b
    base_url: %s/pro
    api_naming: underscore
    auth: {style: header, header: X-API-Key, key: test-key}
    limit: {default: 1000, halve_on_reject: true}
  relay-a:
    kind: tushare-http
    base_url: http://127.0.0.1:1/tushare
    api_naming: hyphen
    auth: {style: header, header: X-API-Key, key: test-key}
    disabled: true
bindings:
  trade_cal: [relay-b, relay-a]
`, srv.URL)
	path := filepath.Join(dir, "sources.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := LoadRegistry(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer reg.Close()

	infos := reg.Infos()
	if len(infos) != 2 {
		t.Fatalf("infos = %+v", infos)
	}
	// 禁用的源在链上被跳过,只剩 relay-b
	chain, err := reg.Resolve("trade_cal")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(chain) != 1 || chain[0].Name() != "relay-b" {
		t.Fatalf("chain = %+v", chain)
	}
	// 未绑定数据集报错
	if _, err := reg.Resolve("bars_daily"); err == nil {
		t.Fatal("expected error for unbound dataset")
	}
	// 通过禁用源获取应报错
	if _, err := reg.Get("relay-a"); err == nil {
		t.Fatal("expected disabled source error")
	}
}
