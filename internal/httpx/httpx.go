// Package httpx 提供数据服务与回测服务共用的 HTTP 中间件。
//
// 只依赖标准库:两个服务的路由都很薄(单端点信封分发 + 2 条 REST 路由),
// 引 Web 框架的收益有限,但"框架自带"的运维硬化必须有:
// panic 恢复、访问日志、请求体上限、超时与优雅停机(见 cmd/quantd 的 serve)。
package httpx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// Middleware 是标准库风格的中间件。
type Middleware func(http.Handler) http.Handler

// Chain 按书写顺序组合中间件(第一个在最外层)。
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// Recover 捕获 handler panic:记录堆栈,并返回 JSON 错误。
//
// 响应形态按路径区分:数据接口(/ 与 /healthz 之外的 tushare 协议)用 tushare 信封,
// 回测 REST(/api/...)用 {"error": ...},保证客户端拿到结构化错误而不是断连。
func Recover(logf func(string, ...any)) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if logf != nil {
						logf("panic %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
					}
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.WriteHeader(http.StatusInternalServerError)
					if strings.HasPrefix(r.URL.Path, "/api/") {
						_ = json.NewEncoder(w).Encode(map[string]any{"error": "internal server error"})
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"code": -1, "msg": "internal server error", "data": nil,
					})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// AccessLog 记录方法、路径、状态码、耗时与响应字节数。
func AccessLog(logf func(string, ...any)) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			if logf != nil {
				logf("%s %s %d %dB %.1fms", r.Method, r.URL.Path, rec.status, rec.bytes,
					float64(time.Since(start).Microseconds())/1000)
			}
		})
	}
}

// MaxBody 限制请求体大小(超限时由解码方返回错误;此处提前拒绝并给 413)。
func MaxBody(n int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// statusRecorder 记录状态码与响应字节数。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

var _ = fmt.Sprintf
