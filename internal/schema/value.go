package schema

import (
	"fmt"
	"time"
)

// ValueKind 是值的逻辑种类,与 FieldType 对应。
type ValueKind uint8

const (
	KindNull ValueKind = iota
	KindString
	KindDate      // I = days since Unix epoch
	KindTimestamp // I = microseconds since Unix epoch
	KindFloat     // F = value
	KindInt       // I = value
	KindBool      // B = value
)

// Value 是行中的单个值。使用固定结构而非 any,避免海量行场景的装箱开销。
type Value struct {
	Kind ValueKind
	F    float64
	I    int64
	S    string
	B    bool
}

func NullValue() Value             { return Value{Kind: KindNull} }
func Float(v float64) Value        { return Value{Kind: KindFloat, F: v} }
func Str(v string) Value           { return Value{Kind: KindString, S: v} }
func Date(days int64) Value        { return Value{Kind: KindDate, I: days} }
func Timestamp(micros int64) Value { return Value{Kind: KindTimestamp, I: micros} }
func Int(v int64) Value            { return Value{Kind: KindInt, I: v} }
func Bool(v bool) Value            { return Value{Kind: KindBool, B: v} }

// IsNull 判断值是否为空。
func (v Value) IsNull() bool { return v.Kind == KindNull }

// Float64 尝试以浮点读取数值,非数值返回 0 和 false。
func (v Value) Float64() (float64, bool) {
	switch v.Kind {
	case KindFloat:
		return v.F, true
	case KindInt:
		return float64(v.I), true
	default:
		return 0, false
	}
}

// Compare 比较同类型值,返回 -1/0/1。类型不同时报错。
// Null 排在任何非 Null 值之前,便于范围比较。
func Compare(a, b Value) (int, error) {
	if a.Kind == KindNull || b.Kind == KindNull {
		switch {
		case a.Kind == KindNull && b.Kind == KindNull:
			return 0, nil
		case a.Kind == KindNull:
			return -1, nil
		default:
			return 1, nil
		}
	}
	switch a.Kind {
	case KindFloat:
		if b.Kind != KindFloat {
			return 0, fmt.Errorf("compare: float vs %v", b.Kind)
		}
		switch {
		case a.F < b.F:
			return -1, nil
		case a.F > b.F:
			return 1, nil
		default:
			return 0, nil
		}
	case KindString:
		if b.Kind != KindString {
			return 0, fmt.Errorf("compare: string vs %v", b.Kind)
		}
		switch {
		case a.S < b.S:
			return -1, nil
		case a.S > b.S:
			return 1, nil
		default:
			return 0, nil
		}
	case KindDate, KindTimestamp, KindInt:
		if b.Kind != a.Kind {
			return 0, fmt.Errorf("compare: %v vs %v", a.Kind, b.Kind)
		}
		switch {
		case a.I < b.I:
			return -1, nil
		case a.I > b.I:
			return 1, nil
		default:
			return 0, nil
		}
	case KindBool:
		if b.Kind != KindBool {
			return 0, fmt.Errorf("compare: bool vs %v", b.Kind)
		}
		switch {
		case !a.B && b.B:
			return -1, nil
		case a.B && !b.B:
			return 1, nil
		default:
			return 0, nil
		}
	default:
		return 0, fmt.Errorf("compare: unsupported kind %v", a.Kind)
	}
}

const (
	secondsPerDay = 86400
)

// DaysFromTime 把时间转换为 epoch 天数(按 UTC 日期)。
func DaysFromTime(t time.Time) int64 {
	return t.Unix() / secondsPerDay
}

// TimeFromDays 把 epoch 天数转换为 UTC 时间(当日零点)。
func TimeFromDays(days int64) time.Time {
	return time.Unix(days*secondsPerDay, 0).UTC()
}

// ParseDate 解析 "YYYYMMDD" 为 epoch 天数,也接受 "YYYY-MM-DD"。
func ParseDate(s string) (int64, error) {
	layouts := []string{"20060102", "2006-01-02", "2006/01/02"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return DaysFromTime(t), nil
		}
	}
	return 0, fmt.Errorf("invalid date %q (expect YYYYMMDD)", s)
}

// FormatDate 把 epoch 天数格式化为 "YYYYMMDD"。
func FormatDate(days int64) string {
	return TimeFromDays(days).Format("20060102")
}

// FormatDateISO 把 epoch 天数格式化为 "YYYY-MM-DD"。
func FormatDateISO(days int64) string {
	return TimeFromDays(days).Format("2006-01-02")
}

// FormatTimestamp 把 micros 格式化为 "YYYY-MM-DD HH:MM:SS"。
func FormatTimestamp(micros int64) string {
	return time.UnixMicro(micros).UTC().Format("2006-01-02 15:04:05")
}

// TimeFromMicros 把 micros 转换为 UTC 时间。
func TimeFromMicros(micros int64) time.Time {
	return time.UnixMicro(micros).UTC()
}

// MicrosFromTime 把时间转换为 micros。
func MicrosFromTime(t time.Time) int64 {
	return t.UnixMicro()
}
