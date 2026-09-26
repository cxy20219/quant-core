package source

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Registry 是数据源插件注册表:从 sources.yaml 载入源与"数据集→源"绑定。
//
// 装载/卸载语义:
//   - 新增源 = 在 sources.yaml 增加一段配置(或放一个新的 exec 插件脚本);
//   - 卸载源 = disabled: true(保留配置)或删除配置段;
//   - 换源   = 修改 bindings 中该数据集的源顺序;
//   - 每次命令(import/check)重新加载,不依赖进程重启。
type Registry struct {
	path     string
	sources  map[string]Source
	infos    map[string]Info
	bindings map[string][]string
}

// Info 是源的元信息(不触发构造)。
type Info struct {
	Name        string
	Kind        string
	Description string
	Disabled    bool
	Err         error // 构造失败原因(仍出现在列表中,便于排查)
}

type registryFile struct {
	Version  int                       `yaml:"version"`
	Sources  map[string]map[string]any `yaml:"sources"`
	Bindings map[string][]string       `yaml:"bindings"`
}

// LoadRegistry 加载 sources.yaml 并构造可用源。
func LoadRegistry(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read sources config: %w", err)
	}
	var file registryFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse sources config: %w", err)
	}
	if file.Version != 1 {
		return nil, fmt.Errorf("sources config version %d not supported (expect 1)", file.Version)
	}
	reg := &Registry{
		path:     path,
		sources:  map[string]Source{},
		infos:    map[string]Info{},
		bindings: map[string][]string{},
	}
	names := make([]string, 0, len(file.Sources))
	for name := range file.Sources {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		cfg := Config{raw: file.Sources[name], path: "sources." + name}
		info := Info{
			Name:        name,
			Kind:        cfg.Str("kind", ""),
			Description: cfg.Str("description", ""),
			Disabled:    cfg.Bool("disabled", false),
		}
		if info.Disabled {
			reg.infos[name] = info
			continue
		}
		factory, ok := factories[info.Kind]
		if !ok {
			info.Err = fmt.Errorf("未知的 kind %q(已注册: %s)", info.Kind, strings.Join(Kinds(), ", "))
			reg.infos[name] = info
			continue
		}
		src, err := factory(name, cfg)
		if err != nil {
			info.Err = err
			reg.infos[name] = info
			continue
		}
		reg.sources[name] = src
		reg.infos[name] = info
	}
	for dataset, chain := range file.Bindings {
		reg.bindings[dataset] = chain
	}
	return reg, nil
}

// Path 返回配置文件路径。
func (r *Registry) Path() string { return r.path }

// Get 按名取源。
func (r *Registry) Get(name string) (Source, error) {
	src, ok := r.sources[name]
	if !ok {
		if info, known := r.infos[name]; known {
			if info.Disabled {
				return nil, fmt.Errorf("源 %q 已禁用(disabled: true)", name)
			}
			if info.Err != nil {
				return nil, fmt.Errorf("源 %q 不可用: %w", name, info.Err)
			}
		}
		return nil, fmt.Errorf("源 %q 未配置", name)
	}
	return src, nil
}

// Resolve 返回数据集可用的源链(按 bindings 顺序,跳过禁用/构造失败的源)。
// dataset 为空时返回全部可用源(按名称排序)。
func (r *Registry) Resolve(dataset string) ([]Source, error) {
	if dataset == "" {
		names := make([]string, 0, len(r.sources))
		for name := range r.sources {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]Source, 0, len(names))
		for _, name := range names {
			out = append(out, r.sources[name])
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("没有可用数据源")
		}
		return out, nil
	}
	chain, ok := r.bindings[dataset]
	if !ok || len(chain) == 0 {
		return nil, fmt.Errorf("数据集 %q 未配置源绑定(bindings),请在 sources.yaml 中补充", dataset)
	}
	var out []Source
	var problems []string
	for _, name := range chain {
		if src, ok := r.sources[name]; ok {
			out = append(out, src)
			continue
		}
		if info, known := r.infos[name]; known {
			reason := "已禁用"
			if info.Err != nil {
				reason = info.Err.Error()
			}
			problems = append(problems, fmt.Sprintf("%s(%s)", name, reason))
		} else {
			problems = append(problems, name+"(未配置)")
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("数据集 %q 的源链全部不可用: %s", dataset, strings.Join(problems, ", "))
	}
	return out, nil
}

// Bindings 返回数据集到源链的映射(副本)。
func (r *Registry) Bindings() map[string][]string {
	out := make(map[string][]string, len(r.bindings))
	for k, v := range r.bindings {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// Infos 返回全部源的元信息(按名称排序)。
func (r *Registry) Infos() []Info {
	out := make([]Info, 0, len(r.infos))
	for _, info := range r.infos {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LoggerSetter 由支持注入日志的源实现(便于观测重试与降级)。
type LoggerSetter interface {
	SetLogger(logf func(format string, args ...any))
}

// SetSourceLoggers 为注册表内全部源注入日志回调。
func (r *Registry) SetSourceLoggers(logf func(format string, args ...any)) {
	for _, src := range r.sources {
		if setter, ok := src.(LoggerSetter); ok {
			setter.SetLogger(logf)
		}
	}
}

// Close 关闭全部源。
func (r *Registry) Close() error {
	var firstErr error
	for _, src := range r.sources {
		if err := src.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
