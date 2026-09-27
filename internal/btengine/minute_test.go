package btengine

import (
	"testing"

	"quant-core/internal/lake"
	"quant-core/internal/schema"
)

// TestMinutePortalLoad 验证分钟门户能在本地湖上加载与命中分钟 Bar。
// 仅在本机存在 D:\quant-lake 时运行。
func TestMinutePortalLoad(t *testing.T) {
	reg, err := schema.Load(`..\..\schemas\datasets.yaml`)
	if err != nil {
		t.Skip("registry unavailable:", err)
	}
	l := lake.New(`D:\quant-lake`, reg)
	day, _ := schema.ParseDate("20200102")
	p := NewMinutePortal(l, []string{"600570.SH"}, 5)
	if err := p.EnsureDay(day); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	bars := p.bars["600570.SH"]
	t.Logf("loaded bars: %d", len(bars))
	if len(bars) == 0 {
		t.Fatal("no minute bars loaded")
	}
	byDay := map[int64]int{}
	for _, bar := range bars {
		byDay[MicrosToDay(bar.Micros)]++
	}
	for d, n := range byDay {
		t.Logf("  day %s: %d bars", schema.FormatDateISO(d), n)
	}
	t.Logf("first bar: %+v", bars[0])
	t.Logf("last bar: %+v", bars[len(bars)-1])
	// 命中 2020-01-02 09:31
	micros := (day*86400 + 9*3600 + 31*60) * 1_000_000
	bar, ok := p.BarAt("600570.SH", micros)
	if !ok {
		t.Fatalf("09:31 bar not found; first=%+v", bars[0])
	}
	t.Logf("09:31 close=%.2f", bar.Close)
	// 分钟序列
	up := p.BarsUpTo("600570.SH", micros, 3)
	if len(up) != 3 {
		t.Fatalf("BarsUpTo got %d bars", len(up))
	}
}
