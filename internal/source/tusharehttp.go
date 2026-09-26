package source

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultRejectHints 是"限额类拒绝"的默认特征词(与配置的 markers 合并)。
var defaultRejectHints = []string{
	"exceeds the limit",
	"too large",
	"upstream_pool_exhausted",
	"response estimate",
}

// TushareHTTP 是 tushare 协议通用源插件(kind: tushare-http)。
//
// 覆盖官方接口与各类中转站;差异全部由配置表达:
//
//	base_url / method(GET|POST)
//	api_naming(underscore|hyphen)+ api_map 覆盖
//	auth(header/token_body/none)
//	limit(default/max/min/halve_on_reject/reject_markers)
//	tls(insecure_skip_verify)/ timeout_ms / retry
//	probe(detect: 识别"5 行样例"伪数据,归为错误)
//
// 响应解析兼容两种信封:
//	官方/A 站: {code, msg, data:{fields, items, count, has_more}}
//	B 站:      {code, msg, data:{fields, items}, count, meta} 及 {ok:false,error,message}
type TushareHTTP struct {
	name       string
	baseURL    string
	method     string
	apiNaming  string
	apiMap     map[string]string
	authStyle  string
	authHeader string
	authKey    string
	limitParam string
	limitDef   int
	limitMax   int
	limitMin   int
	halve      bool
	rejectHint []string
	skipVerify bool
	detectProb bool
	timeout    time.Duration
	retries    int
	backoff    time.Duration
	client     *http.Client
	logf       func(format string, args ...any)
}

// NewTushareHTTP 按配置构造源。
func NewTushareHTTP(name string, cfg Config) (Source, error) {
	if err := cfg.Require("base_url"); err != nil {
		return nil, err
	}
	s := &TushareHTTP{
		name:       name,
		baseURL:    strings.TrimRight(cfg.Str("base_url", ""), "/") + "/",
		method:     strings.ToUpper(cfg.Str("method", http.MethodGet)),
		apiNaming:  cfg.Str("api_naming", "underscore"),
		apiMap:     cfg.StringMap("api_map"),
		authStyle:  cfg.Str("auth.style", "none"),
		authHeader: cfg.Str("auth.header", "X-API-Key"),
		authKey:    cfg.Str("auth.key", ""),
		limitParam: cfg.Str("limit.param", "limit"),
		limitDef:   cfg.Int("limit.default", 5000),
		limitMax:   cfg.Int("limit.max", 0),
		limitMin:   cfg.Int("limit.min", 500),
		halve:      cfg.Bool("limit.halve_on_reject", false),
		rejectHint: append(append([]string{}, defaultRejectHints...), cfg.Strings("limit.reject_markers")...),
		skipVerify: cfg.Bool("tls.insecure_skip_verify", false),
		detectProb: cfg.Bool("probe.detect", false),
		timeout:    time.Duration(cfg.Int("timeout_ms", 30000)) * time.Millisecond,
		retries:    cfg.Int("retry.max", 2),
		backoff:    time.Duration(cfg.Int("retry.backoff_ms", 800)) * time.Millisecond,
	}
	if s.authStyle == "header" && s.authKey == "" {
		return nil, fmt.Errorf("sources.%s: auth.style=header 需要 auth.key(可用 ${ENV} 引用)", name)
	}
	if s.authStyle == "token_body" && s.authKey == "" {
		return nil, fmt.Errorf("sources.%s: auth.style=token_body 需要 auth.key(可用 ${ENV} 引用)", name)
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConnsPerHost: 4,
	}
	if s.skipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 自签证书的中转站
	}
	s.client = &http.Client{Timeout: s.timeout, Transport: transport}
	return s, nil
}

// SetLogger 注入日志回调。
func (s *TushareHTTP) SetLogger(logf func(format string, args ...any)) { s.logf = logf }

func (s *TushareHTTP) Name() string { return s.name }
func (s *TushareHTTP) Kind() string { return "tushare-http" }
func (s *TushareHTTP) Close() error {
	s.client.CloseIdleConnections()
	return nil
}

func (s *TushareHTTP) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// apiName 把标准接口名转换为该源的接口名。
func (s *TushareHTTP) apiName(api string) string {
	if over, ok := s.apiMap[api]; ok {
		return over
	}
	switch s.apiNaming {
	case "hyphen":
		return strings.ReplaceAll(api, "_", "-")
	default:
		return api
	}
}

// errClass 是错误分类。
type errClass uint8

const (
	errFatal   errClass = iota // 参数错误等,重试无意义
	errHalve                    // 限额类错误,可减小 limit 重试
	errTransient                // 限流/服务端瞬时错误,可退避重试
)

// callError 携带错误分类。
type callError struct {
	class  errClass
	err    error
	status int
}

func (e *callError) Error() string { return e.err.Error() }
func (e *callError) Unwrap() error { return e.err }

// Call 执行一次请求,内置"限额减半"与"瞬时退避"重试。
//
// 重试策略(针对中转站实测行为):
//   - 限额类失败(400 预估超限 / 503 上游池耗尽)先在**同一 limit** 重试,
//     连续两次失败才减半——中转站的 503 多为瞬时,过早减半会拿到被截断的数据;
//   - 瞬时类失败(429/5xx/网络)按退避重试。
func (s *TushareHTTP) Call(ctx context.Context, api string, params map[string]any, fields string) (*Result, error) {
	limit := s.limitDef
	if custom, ok := numericParam(params, s.limitParam); ok {
		limit = custom
	}
	if s.limitMax > 0 && limit > s.limitMax {
		limit = s.limitMax
	}
	if limit < s.limitMin {
		limit = s.limitMin
	}

	var lastErr error
	sameLimitFails := 0
	for attempt := 0; attempt <= s.retries; attempt++ {
		result, err := s.callOnce(ctx, api, params, fields, limit)
		if err == nil {
			return result, nil
		}
		lastErr = err
		cls := errFatal
		if ce, ok := err.(*callError); ok {
			cls = ce.class
		}

		canRetry := attempt < s.retries
		switch {
		case cls == errHalve && s.halve:
			sameLimitFails++
			if sameLimitFails < 2 && canRetry {
				// 先在同一 limit 重试一次(中转站瞬时 503 常见)
				s.log("source=%s api=%s limit=%d 失败(%v),同 limit 重试", s.name, api, limit, err)
				if !s.sleep(ctx, attempt) {
					return nil, ctx.Err()
				}
				continue
			}
			if limit > s.limitMin && canRetry {
				newLimit := limit / 2
				if newLimit < s.limitMin {
					newLimit = s.limitMin
				}
				s.log("source=%s api=%s limit=%d 连续失败(%v),降为 %d 重试", s.name, api, limit, err, newLimit)
				limit = newLimit
				sameLimitFails = 0
				if !s.sleep(ctx, attempt) {
					return nil, ctx.Err()
				}
				continue
			}
			// 已达最小 limit:退化为瞬时重试
			if canRetry {
				s.log("source=%s api=%s 已达最小 limit 仍失败(%v),退避重试", s.name, api, err)
				if !s.sleep(ctx, attempt) {
					return nil, ctx.Err()
				}
				continue
			}
			return nil, err
		case (cls == errTransient) && canRetry:
			s.log("source=%s api=%s 第 %d 次重试(%v)", s.name, api, attempt+1, err)
			if !s.sleep(ctx, attempt) {
				return nil, ctx.Err()
			}
			continue
		default:
			return nil, err
		}
	}
	return nil, lastErr
}

// sleep 按轮次退避;返回 false 表示 ctx 已取消。
func (s *TushareHTTP) sleep(ctx context.Context, attempt int) bool {
	delay := s.backoff * time.Duration(attempt+1)
	if delay > 20*time.Second {
		delay = 20 * time.Second
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
}

func (s *TushareHTTP) callOnce(ctx context.Context, api string, params map[string]any, fields string, limit int) (*Result, error) {
	path := s.baseURL + s.apiName(api)
	query := url.Values{}
	if s.method == http.MethodGet {
		for key, value := range params {
			if value == nil {
				continue
			}
			query.Set(key, fmt.Sprint(value))
		}
		if fields != "" {
			query.Set("fields", fields)
		}
		query.Set(s.limitParam, strconv.Itoa(limit))
		path += "?" + query.Encode()
	}

	var body io.Reader
	if s.method == http.MethodPost {
		payload := map[string]any{}
		for key, value := range params {
			payload[key] = value
		}
		if fields != "" {
			payload["fields"] = fields
		}
		payload[s.limitParam] = limit
		if s.authStyle == "token_body" {
			payload["token"] = s.authKey
			if _, ok := payload["api_name"]; !ok {
				payload["api_name"] = api
			}
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, &callError{class: errFatal, err: err}
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, s.method, path, body)
	if err != nil {
		return nil, &callError{class: errFatal, err: err}
	}
	if s.method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.authStyle == "header" {
		req.Header.Set(s.authHeader, s.authKey)
	}
	req.Header.Set("User-Agent", "quant-core/0.1")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, &callError{class: errTransient, err: fmt.Errorf("http: %w", err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, &callError{class: errTransient, err: fmt.Errorf("read body: %w", err)}
	}
	status := resp.StatusCode

	result, parseErr := parseEnvelope(raw)
	if parseErr == nil {
		result.Limit = limit
		if s.detectProb && isProbe(result.Meta) {
			return nil, &callError{class: errFatal, err: fmt.Errorf(
				"source %s: 接口 %s 返回 probe 样例(缺少内容参数或未带 limit),不能作为数据使用", s.name, api)}
		}
		return result, nil
	}

	// 是否需要降级/重试:看错误文本与 HTTP 状态
	msg := parseErr.Error() + " " + truncateStr(string(raw), 400)
	switch {
	case s.halve && (status == http.StatusBadRequest || status == http.StatusServiceUnavailable) && containsAny(msg, s.rejectHint):
		return nil, &callError{class: errHalve, err: parseErr, status: status}
	case status == http.StatusTooManyRequests || status >= 500 || status == http.StatusServiceUnavailable:
		return nil, &callError{class: errTransient, err: parseErr, status: status}
	default:
		return nil, &callError{class: errFatal, err: parseErr, status: status}
	}
}

// envelope 兼容官方/A 站/B 站三种响应结构。
type envelope struct {
	Code    int    `json:"code"`
	Msg     string `json:"msg"`
	Detail  string `json:"detail"`
	OK      *bool  `json:"ok"`
	Error   string `json:"error"`
	Message string `json:"message"`
	Data    *struct {
		Fields  []string `json:"fields"`
		Items   [][]any  `json:"items"`
		Count   *int64   `json:"count"`
		HasMore *bool    `json:"has_more"`
	} `json:"data"`
	Count *int64         `json:"count"`
	Meta  map[string]any `json:"meta"`
}

func parseEnvelope(raw []byte) (*Result, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("响应不是合法 JSON: %w", err)
	}
	if env.OK != nil && !*env.OK {
		msg := env.Message
		if msg == "" {
			msg = env.Error
		}
		if env.Error != "" && env.Message != "" {
			msg = env.Error + ": " + env.Message
		}
		return nil, &SrcError{Msg: msg}
	}
	if env.Code != 0 {
		return nil, &SrcError{Code: env.Code, Msg: strings.TrimSpace(env.Msg + " " + env.Detail)}
	}
	if env.Data == nil {
		return nil, fmt.Errorf("响应缺少 data 字段")
	}
	result := &Result{
		Fields: env.Data.Fields,
		Items:  env.Data.Items,
		Meta:   env.Meta,
	}
	if env.Data.Count != nil {
		result.Count = *env.Data.Count
	} else if env.Count != nil {
		result.Count = *env.Count
	}
	if env.Data.HasMore != nil {
		result.HasMore = *env.Data.HasMore
	} else if result.Count > 0 {
		result.HasMore = int64(len(result.Items)) < result.Count
	}
	return result, nil
}

// isProbe 判断 B 站 meta.probe / meta.sample 标记。
func isProbe(meta map[string]any) bool {
	if meta == nil {
		return false
	}
	if v, ok := meta["probe"].(bool); ok && v {
		return true
	}
	if v, ok := meta["sample"].(bool); ok && v {
		return true
	}
	return false
}

func containsAny(text string, hints []string) bool {
	lower := strings.ToLower(text)
	for _, hint := range hints {
		if hint != "" && strings.Contains(lower, strings.ToLower(hint)) {
			return true
		}
	}
	return false
}

func numericParam(params map[string]any, key string) (int, bool) {
	v, ok := params[key]
	if !ok || v == nil {
		return 0, false
	}
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return n, true
		}
	}
	return 0, false
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
