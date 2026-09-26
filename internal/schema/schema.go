// Package schema 定义数据集注册表:字段、类型、分区、主键与写入参数。
//
// 注册表是数据湖的单一权威描述,所有组件(查询服务/迁移器/回测引擎/导入器)
// 都必须从这里获取数据集定义,不允许在代码中硬编码。
package schema

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FieldType 是字段的逻辑类型。物理存储映射由 lake 写入器与查询扫描器负责:
//
//	string    -> BYTE_ARRAY (UTF8)
//	date      -> INT32 DATE (days since epoch)
//	timestamp -> INT64 TIMESTAMP (micros since epoch)
//	float64   -> DOUBLE
//	int64     -> INT64
//	bool      -> BOOLEAN
type FieldType string

const (
	TypeString    FieldType = "string"
	TypeDate      FieldType = "date"
	TypeTimestamp FieldType = "timestamp"
	TypeFloat64   FieldType = "float64"
	TypeInt64     FieldType = "int64"
	TypeBool      FieldType = "bool"
)

// Field 描述数据集的一个字段。
type Field struct {
	Name string    `yaml:"name"`
	Type FieldType `yaml:"type"`
	Unit string    `yaml:"unit,omitempty"`
	Desc string    `yaml:"desc,omitempty"`
}

// WriterConfig 控制写入时的行组与文件切分。
type WriterConfig struct {
	// RowGroupRows 是单个行组的最大行数。
	RowGroupRows int64 `yaml:"row_group_rows"`
	// TargetFileRows 是单个 part 文件的目标行数上限,超出则切分新文件。
	TargetFileRows int64 `yaml:"target_file_rows"`
}

// DefaultWriterConfig 返回默认写入参数。
func DefaultWriterConfig() WriterConfig {
	return WriterConfig{RowGroupRows: 131072, TargetFileRows: 2_000_000}
}

// Dataset 是单个数据集的完整定义。
type Dataset struct {
	Name        string       `yaml:"-"`
	Description string       `yaml:"description"`
	Root        string       `yaml:"root"`
	Partitions  []string     `yaml:"partitions"`
	PrimaryKey  []string     `yaml:"primary_key"`
	SortedBy    []string     `yaml:"sorted_by"`
	Writer      WriterConfig `yaml:"writer"`
	Fields      []Field      `yaml:"fields"`

	index map[string]int
}

// FieldIndex 返回字段在 Fields 中的位置。
func (d *Dataset) FieldIndex(name string) (int, bool) {
	i, ok := d.index[name]
	return i, ok
}

// Field 按名查找字段。
func (d *Dataset) FieldByName(name string) (Field, bool) {
	if i, ok := d.index[name]; ok {
		return d.Fields[i], true
	}
	return Field{}, false
}

// FieldNames 返回全部字段名(按定义顺序)。
func (d *Dataset) FieldNames() []string {
	names := make([]string, len(d.Fields))
	for i, f := range d.Fields {
		names[i] = f.Name
	}
	return names
}

// TimeField 返回第一个 date/timestamp 类型字段名(用于分区推导),没有则返回空串。
func (d *Dataset) TimeField() string {
	for _, f := range d.Fields {
		if f.Type == TypeDate || f.Type == TypeTimestamp {
			return f.Name
		}
	}
	return ""
}

// Registry 是全部数据集的集合。
type Registry struct {
	Version  int                 `yaml:"version"`
	Datasets map[string]*Dataset `yaml:"datasets"`
}

// Get 按名取数据集。
func (r *Registry) Get(name string) (*Dataset, error) {
	ds, ok := r.Datasets[name]
	if !ok {
		return nil, fmt.Errorf("unknown dataset %q", name)
	}
	return ds, nil
}

// Names 返回排序后的数据集名列表。
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.Datasets))
	for name := range r.Datasets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Load 从 YAML 文件加载注册表并校验。
func Load(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	return Parse(data)
}

// Parse 从 YAML 内容解析注册表并校验。
func Parse(data []byte) (*Registry, error) {
	var reg Registry
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	if reg.Version != 1 {
		return nil, fmt.Errorf("unsupported registry version %d (expect 1)", reg.Version)
	}
	if len(reg.Datasets) == 0 {
		return nil, fmt.Errorf("registry has no datasets")
	}
	for name, ds := range reg.Datasets {
		ds.Name = name
		if err := ds.validate(); err != nil {
			return nil, fmt.Errorf("dataset %s: %w", name, err)
		}
	}
	return &reg, nil
}

func (d *Dataset) validate() error {
	if strings.TrimSpace(d.Root) == "" {
		return fmt.Errorf("root is required")
	}
	if len(d.Fields) == 0 {
		return fmt.Errorf("fields are required")
	}
	d.index = make(map[string]int, len(d.Fields))
	for i, f := range d.Fields {
		if f.Name == "" {
			return fmt.Errorf("field %d has empty name", i)
		}
		if _, dup := d.index[f.Name]; dup {
			return fmt.Errorf("duplicate field %q", f.Name)
		}
		switch f.Type {
		case TypeString, TypeDate, TypeTimestamp, TypeFloat64, TypeInt64, TypeBool:
		default:
			return fmt.Errorf("field %q has unsupported type %q", f.Name, f.Type)
		}
		d.index[f.Name] = i
	}
	for _, key := range append(append([]string{}, d.PrimaryKey...), d.SortedBy...) {
		if _, ok := d.index[key]; !ok {
			return fmt.Errorf("key %q is not a field", key)
		}
	}
	for _, p := range d.Partitions {
		if _, clash := d.index[p]; clash {
			return fmt.Errorf("partition %q must not be a field", p)
		}
	}
	if err := d.Writer.validate(); err != nil {
		return err
	}
	return nil
}

func (w *WriterConfig) validate() error {
	if w.RowGroupRows == 0 && w.TargetFileRows == 0 {
		*w = DefaultWriterConfig()
		return nil
	}
	if w.RowGroupRows < 0 || w.TargetFileRows < 0 {
		return fmt.Errorf("writer config must be positive")
	}
	if w.RowGroupRows == 0 {
		w.RowGroupRows = DefaultWriterConfig().RowGroupRows
	}
	if w.TargetFileRows == 0 {
		w.TargetFileRows = DefaultWriterConfig().TargetFileRows
	}
	return nil
}

// PartitionSpec 返回分区键列表。
func (d *Dataset) PartitionSpec() []string { return d.Partitions }

// HasPartition 判断数据集是否按指定键分区。
func (d *Dataset) HasPartition(key string) bool {
	for _, p := range d.Partitions {
		if p == key {
			return true
		}
	}
	return false
}
