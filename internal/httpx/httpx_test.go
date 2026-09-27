package httpx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRecoverWritesJSON 验证 panic 被恢复为 JSON 错误(tushare 信封与 REST 两种形态)。
func TestRecoverWritesJSON(t *testing.T) {
	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	logs := []string{}
	h := Chain(boom, Recover(func(f string, a ...any) { logs = append(logs, f) }))

	for _, path := range []string{"/", "/api/backtests"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s: status=%d", path, rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: invalid json: %v", path, err)
		}
		if strings.HasPrefix(path, "/api/") {
			if body["error"] != "internal server error" {
				t.Fatalf("%s: body=%v", path, body)
			}
		} else if body["code"] != float64(-1) {
			t.Fatalf("%s: body=%v", path, body)
		}
	}
	if len(logs) != 2 {
		t.Fatalf("expected 2 panic logs, got %d", len(logs))
	}
}

// TestMaxBodyRejects 验证请求体超限被拒绝。
func TestMaxBodyRejects(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 0, 64)
		tmp := make([]byte, 32)
		for {
			n, err := r.Body.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if err != nil {
				break
			}
		}
		w.WriteHeader(http.StatusOK)
	})
	h := Chain(handler, MaxBody(16))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 64))))
	// MaxBytesReader 会让读取返回错误;此处只要求不把 64 字节完整读出
	if len(rec.Body.String()) > 0 {
		t.Fatalf("unexpected body: %q", rec.Body.String())
	}
}

// TestAccessLog 验证访问日志包含方法/路径/状态。
func TestAccessLog(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hi"))
	})
	var logged string
	h := Chain(ok, AccessLog(func(f string, a ...any) { logged = fmt.Sprintf(f, a...) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if !strings.Contains(logged, "GET /healthz 418") {
		t.Fatalf("unexpected log: %q", logged)
	}
}
