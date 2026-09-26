// Package source 提供可插拔的数据源(数据获取插件)框架。
//
// 设计目标:数据源可以随时装载与卸载,不重新编译主程序。
//   - 每个源由 sources.yaml 中的一段配置声明;kind 决定用哪个插件实现;
//     disabled: true 即可"卸载"而不删除配置。
//   - bindings 定义"数据集 → 源优先级",同一数据集可配置降级链。
//   - 内置 kind:tushare-http(官方与各类 tushare 协议中转站)、exec(外部命令插件)。
//
// 约定:调用方一律使用 tushare 标准接口名(下划线),由各源插件负责
// 命名转换、认证、分页上限与错误语义。
package source

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Result 是一次数据请求的规范化结果。
type Result struct {
	Fields  []string       // 列名
	Items   [][]any        // 行(与 Fields 对齐)
	Count   int64          // 源声明的总条数;0 表示未知
	HasMore bool           // 是否还有更多数据
	Limit   int            // 本次请求实际使用的 limit(用于"触顶=可能截断"检测)
	Meta    map[string]any // 源附加元数据(如 B 站的 probe 标记)
}

// SrcError 是源返回的业务错误,err 中保留原始错误码与消息。
type SrcError struct {
	Code int
	Msg  string
}

func (e *SrcError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("source error code=%d: %s", e.Code, e.Msg)
	}
	return "source error: " + e.Msg
}

// Source 是数据源插件。
type Source interface {
	// Name 是注册名(sources.yaml 的键)。
	Name() string
	// Kind 是插件类型。
	Kind() string
	// Call 执行一次数据请求。api 为 tushare 标准接口名;params 为查询参数;
	// fields 为列裁剪(空串表示源默认)。实现需自行处理认证、命名转换、
	// 单次上限与减半重试。
	Call(ctx context.Context, api string, params map[string]any, fields string) (*Result, error)
	// Close 释放资源。
	Close() error
}

// Factory 按配置构造源。
type Factory func(name string, cfg Config) (Source, error)

// factories 是内置插件类型表。
var factories = map[string]Factory{
	"tushare-http": NewTushareHTTP,
	"exec":         NewExec,
}

// RegisterKind 注册新的源插件类型(供扩展或测试使用)。
func RegisterKind(kind string, factory Factory) {
	factories[kind] = factory
}

// Kinds 返回已注册的插件类型。
func Kinds() []string {
	out := make([]string, 0, len(factories))
	for k := range factories {
		out = append(out, k)
	}
	return out
}

// Config 是键值配置的只读包装,支持嵌套键(a.b.c)与环境变量展开。
type Config struct {
	raw  map[string]any
	path string // 用于错误信息,如 "sources.relay-b"
}

// NewConfig 创建配置包装。
func NewConfig(raw map[string]any, path string) Config {
	return Config{raw: raw, path: path}
}

// Path 返回配置路径(报错用)。
func (c Config) Path() string { return c.path }

// Raw 返回底层 map。
func (c Config) Raw() map[string]any { return c.raw }

// Has 判断键是否存在。
func (c Config) Has(key string) bool {
	_, ok := c.lookup(key)
	return ok
}

func (c Config) lookup(key string) (any, bool) {
	parts := strings.Split(key, ".")
	var cur any = c.raw
	for _, part := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Str 读取字符串(空值返回默认;支持 ${ENV} 展开)。
func (c Config) Str(key string, def string) string {
	v, ok := c.lookup(key)
	if !ok || v == nil {
		return def
	}
	s, ok := v.(string)
	if !ok {
		s = fmt.Sprint(v)
	}
	s = os.Expand(strings.TrimSpace(s), func(name string) string {
		if val, ok := os.LookupEnv(name); ok {
			return val
		}
		return ""
	})
	if s == "" {
		return def
	}
	return s
}

// Int 读取整数。
func (c Config) Int(key string, def int) int {
	v, ok := c.lookup(key)
	if !ok || v == nil {
		return def
	}
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return n
		}
	}
	return def
}

// Bool 读取布尔值。
func (c Config) Bool(key string, def bool) bool {
	v, ok := c.lookup(key)
	if !ok || v == nil {
		return def
	}
	switch x := v.(type) {
	case bool:
		return x
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		}
	}
	return def
}

// Strings 读取字符串列表。
func (c Config) Strings(key string) []string {
	v, ok := c.lookup(key)
	if !ok || v == nil {
		return nil
	}
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			out = append(out, fmt.Sprint(item))
		}
		return out
	case []string:
		return x
	case string:
		if strings.TrimSpace(x) == "" {
			return nil
		}
		parts := strings.Split(x, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}

// StringMap 读取字符串字典。
func (c Config) StringMap(key string) map[string]string {
	v, ok := c.lookup(key)
	if !ok || v == nil {
		return nil
	}
	out := map[string]string{}
	switch x := v.(type) {
	case map[string]any:
		for k, item := range x {
			out[k] = fmt.Sprint(item)
		}
	case map[string]string:
		return x
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Child 返回子配置。
func (c Config) Child(key string) Config {
	v, ok := c.lookup(key)
	if !ok {
		return Config{raw: nil, path: c.path + "." + key}
	}
	m, _ := v.(map[string]any)
	return Config{raw: m, path: c.path + "." + key}
}

// Require 校验必填键。
func (c Config) Require(keys ...string) error {
	for _, key := range keys {
		if !c.Has(key) || c.Str(key, "") == "" {
			return fmt.Errorf("%s: missing required config %q", c.path, key)
		}
	}
	return nil
}
