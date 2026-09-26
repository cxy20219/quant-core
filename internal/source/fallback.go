package source

import (
	"context"
	"fmt"
)

// Fallback 把多个源组合成降级链:每次调用按顺序尝试,前一个失败自动切下一个。
//
// 与"整个数据集绑定一个源"相比,按调用降级能避免单次瞬时故障(如中转站的
// 上游池耗尽)导致整批切换源;导入过程仍是同一份规格与同一目标湖。
type Fallback struct {
	name    string
	sources []Source
	logf    func(format string, args ...any)
}

// NewFallback 创建降级链。sources 按优先级排列,至少一个。
func NewFallback(name string, sources []Source) (*Fallback, error) {
	if len(sources) == 0 {
		return nil, fmt.Errorf("fallback %s: 需要至少一个源", name)
	}
	return &Fallback{name: name, sources: sources}, nil
}

// SetLogger 注入日志回调。
func (f *Fallback) SetLogger(logf func(format string, args ...any)) { f.logf = logf }

func (f *Fallback) Name() string { return f.name }
func (f *Fallback) Kind() string { return "fallback" }

// Members 返回链上的源(观测用)。
func (f *Fallback) Members() []Source { return f.sources }

func (f *Fallback) Close() error {
	var firstErr error
	for _, src := range f.sources {
		if err := src.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Call 按优先级依次尝试各源。
func (f *Fallback) Call(ctx context.Context, api string, params map[string]any, fields string) (*Result, error) {
	var lastErr error
	for i, src := range f.sources {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		result, err := src.Call(ctx, api, params, fields)
		if err == nil {
			if i > 0 && f.logf != nil {
				f.logf("降级链 %s: %s 不可用,本次由 %s 提供服务", f.name, f.sources[i-1].Name(), src.Name())
			}
			return result, nil
		}
		lastErr = err
		if f.logf != nil && i < len(f.sources)-1 {
			f.logf("降级链 %s: %s 调用失败(%v),切换下一个源", f.name, src.Name(), err)
		}
	}
	return nil, fmt.Errorf("降级链 %s 全部失败: %w", f.name, lastErr)
}
