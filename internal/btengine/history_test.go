package btengine

import (
	"math"
	"testing"

	"quant-core/internal/lake"
	"quant-core/internal/schema"
)

// TestHistoryFQValues 复现 handle_data 的 get_history(fq=post) 路径,检查非有限值。
// 仅在本机存在 D:\quant-lake 时运行。
func TestHistoryFQValues(t *testing.T) {
	reg, err := schema.Load(`..\..\schemas\datasets.yaml`)
	if err != nil {
		t.Skip("registry unavailable:", err)
	}
	l := lake.New(`D:\quant-lake`, reg)
	start, _ := schema.ParseDate("20240102")
	end, _ := schema.ParseDate("20240110")
	p := NewDataPortal(l, start, end, 365)
	if err := p.EnsureDaily([]string{"600000.SH"}); err != nil {
		t.Fatalf("load: %v", err)
	}
	p.Adfactors([]string{"600000.SH"})
	cal := p.CalendarUpTo(start, 25)
	t.Logf("calendar days: %d, first=%s last=%s", len(cal),
		schema.FormatDateISO(cal[0]), schema.FormatDateISO(cal[len(cal)-1]))

	values := p.HistoryValuesByDates("600000.SH", "close", cal)
	for i, v := range values {
		if math.IsInf(v, 0) || math.IsNaN(v) {
			t.Errorf("base value[%d] (%s) non-finite: %v", i, schema.FormatDateISO(cal[i]), v)
		}
	}
	for i, d := range cal {
		f := p.AdjFactorAt("600000.SH", d)
		if math.IsInf(f, 0) || math.IsNaN(f) || f == 0 {
			t.Errorf("factor[%d] (%s) bad: %v", i, schema.FormatDateISO(d), f)
		}
		v := values[i] * f
		if math.IsInf(v, 0) || math.IsNaN(v) {
			t.Errorf("post value[%d] non-finite: %v (base=%v factor=%v)", i, v, values[i], f)
		}
	}
}
