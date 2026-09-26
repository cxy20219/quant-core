// Package pq 是数据集注册表与 parquet-go 之间的桥接层:
// 构造 parquet schema、转换逻辑值与 parquet 值、解析时间戳单位。
//
// 重要约定:所有字段一律构建为 optional 列,非 null 值 definition level = 1,
// null 值 definition level = 0。写出的行必须遵守此约定。
package pq

import (
	"fmt"
	"sort"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/format"

	"quant-core/internal/schema"
)

// Node 为注册表字段构建 optional parquet 节点。
func Node(f schema.Field) parquet.Node {
	var node parquet.Node
	switch f.Type {
	case schema.TypeString:
		node = parquet.String()
	case schema.TypeDate:
		node = parquet.Date()
	case schema.TypeTimestamp:
		node = parquet.TimestampAdjusted(parquet.Microsecond, false)
	case schema.TypeFloat64:
		node = parquet.Leaf(parquet.DoubleType)
	case schema.TypeInt64:
		node = parquet.Leaf(parquet.Int64Type)
	case schema.TypeBool:
		node = parquet.Leaf(parquet.BooleanType)
	default:
		panic(fmt.Sprintf("pq: unsupported field type %q", f.Type))
	}
	return parquet.Optional(node)
}

// Schema 构建数据集的 parquet schema。
func Schema(ds *schema.Dataset) *parquet.Schema {
	group := parquet.Group{}
	for _, f := range ds.Fields {
		group[f.Name] = Node(f)
	}
	return parquet.NewSchema(ds.Name, group)
}

// FieldOrder 返回按字段名字典序排列的数据集字段索引。
// parquet-go 的 Group.Fields() 按名称排序,这里是与之配套的列顺序约定。
func FieldOrder(ds *schema.Dataset) []int {
	order := make([]int, len(ds.Fields))
	for i := range ds.Fields {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		return ds.Fields[order[i]].Name < ds.Fields[order[j]].Name
	})
	return order
}

// Value 把逻辑值转换为 parquet 值。col 是目标 schema 中的列位置。
func Value(v schema.Value, ft schema.FieldType, col int) parquet.Value {
	if v.IsNull() {
		return parquet.NullValue().Level(0, 0, col)
	}
	var pv parquet.Value
	switch ft {
	case schema.TypeString:
		pv = parquet.ByteArrayValue([]byte(v.S))
	case schema.TypeDate:
		pv = parquet.Int32Value(int32(v.I))
	case schema.TypeTimestamp:
		pv = parquet.Int64Value(v.I)
	case schema.TypeFloat64:
		pv = parquet.DoubleValue(v.F)
	case schema.TypeInt64:
		pv = parquet.Int64Value(v.I)
	case schema.TypeBool:
		pv = parquet.BooleanValue(v.B)
	default:
		return parquet.NullValue().Level(0, 0, col)
	}
	return pv.Level(0, 1, col)
}

// FromParquet 把 parquet 值转换为逻辑值。tsScale 见 TimestampScale。
func FromParquet(pv parquet.Value, ft schema.FieldType, tsScale int64) (schema.Value, error) {
	if pv.IsNull() {
		return schema.NullValue(), nil
	}
	switch ft {
	case schema.TypeString:
		return schema.Str(string(pv.ByteArray())), nil
	case schema.TypeDate:
		return schema.Date(int64(pv.Int32())), nil
	case schema.TypeTimestamp:
		micros := pv.Int64()
		switch {
		case tsScale == 1:
		case tsScale == -1000:
			micros /= 1000
		default:
			micros *= tsScale
		}
		return schema.Timestamp(micros), nil
	case schema.TypeFloat64:
		return schema.Float(pv.Double()), nil
	case schema.TypeInt64:
		return schema.Int(pv.Int64()), nil
	case schema.TypeBool:
		return schema.Bool(pv.Boolean()), nil
	default:
		return schema.NullValue(), fmt.Errorf("pq: unsupported field type %q", ft)
	}
}

// TimestampScale 返回存储值到 micros 的乘数:
// 1 表示已经是 micros,-1000 表示需要除以 1000(nanoseconds),1000 表示 milliseconds。
func TimestampScale(lt *format.LogicalType) int64 {
	if lt == nil {
		return 1
	}
	ts, ok := lt.Value.(*format.TimestampType)
	if !ok {
		return 1
	}
	switch ts.Unit.Value.(type) {
	case *format.MilliSeconds:
		return 1000
	case *format.NanoSeconds:
		return -1000
	default: // MicroSeconds
		return 1
	}
}

// CompareValues 比较两个同物理类型的 parquet 值。
func CompareValues(a, b parquet.Value) int {
	if a.IsNull() || b.IsNull() {
		switch {
		case a.IsNull() && b.IsNull():
			return 0
		case a.IsNull():
			return -1
		default:
			return 1
		}
	}
	switch a.Kind() {
	case parquet.Boolean:
		ab, bb := a.Boolean(), b.Boolean()
		switch {
		case !ab && bb:
			return -1
		case ab && !bb:
			return 1
		}
	case parquet.Int32:
		return compareInt64(int64(a.Int32()), int64(b.Int32()))
	case parquet.Int64:
		return compareInt64(a.Int64(), b.Int64())
	case parquet.Float:
		return compareFloat64(float64(a.Float()), float64(b.Float()))
	case parquet.Double:
		return compareFloat64(a.Double(), b.Double())
	case parquet.ByteArray, parquet.FixedLenByteArray:
		return bytesCompare(a.ByteArray(), b.ByteArray())
	}
	return 0
}

// ToParquet 把逻辑值转换为用于统计比较的 parquet 值(不带 level)。
func ToParquet(v schema.Value, ft schema.FieldType) parquet.Value {
	switch ft {
	case schema.TypeString:
		return parquet.ByteArrayValue([]byte(v.S))
	case schema.TypeDate:
		return parquet.Int32Value(int32(v.I))
	case schema.TypeTimestamp:
		return parquet.Int64Value(v.I)
	case schema.TypeFloat64:
		return parquet.DoubleValue(v.F)
	case schema.TypeInt64:
		return parquet.Int64Value(v.I)
	case schema.TypeBool:
		return parquet.BooleanValue(v.B)
	default:
		return parquet.NullValue()
	}
}

func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func compareFloat64(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func bytesCompare(a, b []byte) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	}
	min := len(a)
	if len(b) < min {
		min = len(b)
	}
	for i := 0; i < min; i++ {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	default:
		return 0
	}
}
