// Package factor 提供因子注册、截面计算与跟踪(IC / 分层收益)。
//
// 设计:
//   - 因子注册表存湖内 meta/factors.json(可增删改,面板与 API 共用同一份);
//   - 内置因子用 Go 原生计算(基于 daily_cross 截面表,单日全市场一次扫描);
//   - 跟踪 = 逐日截面因子值 vs 前向收益的 IC/RankIC + 分层(分位)收益。
package factor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Definition 是一个因子的注册信息。
type Definition struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Kind        string         `json:"kind"`   // 内置因子类型(见 Catalog)
	Params      map[string]any `json:"params"` // 类型参数(如 window)
	Description string         `json:"description,omitempty"`
	// Direction 是因子方向:positive 表示值越大未来收益越高(expected 正 IC)。
	Direction string    `json:"direction,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// Param 描述内置因子的一个参数(用于面板与校验)。
type Param struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Default any    `json:"default"`
	Desc    string `json:"desc"`
}

// Kind 描述一种内置因子类型。
type Kind struct {
	Kind        string  `json:"kind"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Params      []Param `json:"params"`
}

// Catalog 返回内置因子类型目录。
func Catalog() []Kind {
	return []Kind{
		{
			Kind: "momentum", Name: "动量",
			Description: "过去 N 个交易日收益率:close/close[-N]-1",
			Params:      []Param{{Name: "window", Type: "int", Default: 20, Desc: "回看交易日数"}},
		},
		{
			Kind: "reversal", Name: "反转",
			Description: "过去 N 个交易日收益率取负:-momentum(N)",
			Params:      []Param{{Name: "window", Type: "int", Default: 5, Desc: "回看交易日数"}},
		},
		{
			Kind: "volatility", Name: "波动率",
			Description: "过去 N 个交易日日收益标准差(年化前的原始值)",
			Params:      []Param{{Name: "window", Type: "int", Default: 20, Desc: "回看交易日数"}},
		},
		{
			Kind: "turnover", Name: "换手率均值",
			Description: "过去 N 个交易日换手率均值(%)",
			Params:      []Param{{Name: "window", Type: "int", Default: 20, Desc: "回看交易日数"}},
		},
		{
			Kind: "size", Name: "市值",
			Description: "总市值(万元)取对数,小市值因子取负",
			Params:      []Param{{Name: "log", Type: "bool", Default: true, Desc: "是否取自然对数"}},
		},
		{
			Kind: "field", Name: "估值字段",
			Description: "直接取截面表字段(如 pb / pe_ttm / dv_ttm / circ_mv)",
			Params:      []Param{{Name: "field", Type: "string", Default: "pb", Desc: "daily_cross 字段名"}},
		},
	}
}

// Registry 是因子注册表(文件持久化)。
type Registry struct {
	path  string
	items map[string]*Definition
}

// LoadRegistry 读取注册表;文件不存在时返回空表(不报错)。
func LoadRegistry(path string) (*Registry, error) {
	reg := &Registry{path: path, items: map[string]*Definition{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return reg, nil
		}
		return nil, err
	}
	var list []*Definition
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("解析因子注册表 %s: %w", path, err)
	}
	for _, def := range list {
		reg.items[def.ID] = def
	}
	return reg, nil
}

// Save 写回注册表(按 id 排序,便于 diff)。
func (r *Registry) Save() error {
	list := r.List()
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// List 返回全部因子(按 ID 升序)。
func (r *Registry) List() []*Definition {
	out := make([]*Definition, 0, len(r.items))
	for _, def := range r.items {
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get 按 ID 取因子。
func (r *Registry) Get(id string) (*Definition, bool) {
	def, ok := r.items[id]
	return def, ok
}

// Put 新增/更新因子(校验通过后写入并落盘)。
func (r *Registry) Put(def *Definition) error {
	if err := Validate(def); err != nil {
		return err
	}
	if _, exists := r.items[def.ID]; !exists {
		def.CreatedAt = time.Now()
	}
	r.items[def.ID] = def
	return r.Save()
}

// Delete 删除因子。
func (r *Registry) Delete(id string) error {
	if _, ok := r.items[id]; !ok {
		return fmt.Errorf("因子 %s 不存在", id)
	}
	delete(r.items, id)
	return r.Save()
}

// Validate 校验因子定义(id/name/kind/参数类型)。
func Validate(def *Definition) error {
	if strings.TrimSpace(def.ID) == "" {
		return fmt.Errorf("因子 id 不能为空")
	}
	if strings.TrimSpace(def.Name) == "" {
		return fmt.Errorf("因子 name 不能为空")
	}
	var kind *Kind
	for _, k := range Catalog() {
		if k.Kind == def.Kind {
			kk := k
			kind = &kk
			break
		}
	}
	if kind == nil {
		return fmt.Errorf("未知因子类型 %q", def.Kind)
	}
	if def.Direction != "" && def.Direction != "positive" && def.Direction != "negative" {
		return fmt.Errorf("direction 只能是 positive / negative")
	}
	for _, p := range kind.Params {
		v, ok := def.Params[p.Name]
		if !ok {
			continue
		}
		switch p.Type {
		case "int":
			if _, err := asInt(v); err != nil {
				return fmt.Errorf("参数 %s 需为整数: %w", p.Name, err)
			}
		case "bool":
			if _, err := asBool(v); err != nil {
				return fmt.Errorf("参数 %s 需为布尔: %w", p.Name, err)
			}
		}
	}
	return nil
}

// Window 返回因子参数中的窗口(交易日数),缺省用类型默认值。
func (d *Definition) Window() int {
	if v, ok := d.Params["window"]; ok {
		if n, err := asInt(v); err == nil && n > 0 {
			return n
		}
	}
	for _, k := range Catalog() {
		if k.Kind != d.Kind {
			continue
		}
		for _, p := range k.Params {
			if p.Name == "window" {
				if n, err := asInt(p.Default); err == nil && n > 0 {
					return n
				}
			}
		}
	}
	return 20
}

// Field 返回 field 类型因子的字段名。
func (d *Definition) Field() string {
	if v, ok := d.Params["field"].(string); ok && v != "" {
		return v
	}
	return "pb"
}

// LogSize 返回 size 类型因子是否取对数。
func (d *Definition) LogSize() bool {
	if v, ok := d.Params["log"]; ok {
		if b, err := asBool(v); err == nil {
			return b
		}
	}
	return true
}

func asInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	case json.Number:
		i, err := n.Int64()
		return int(i), err
	default:
		return 0, fmt.Errorf("非整数: %v", v)
	}
}

func asBool(v any) (bool, error) {
	switch b := v.(type) {
	case bool:
		return b, nil
	case string:
		return b == "true" || b == "1", nil
	case float64:
		return b != 0, nil
	default:
		return false, fmt.Errorf("非布尔: %v", v)
	}
}
