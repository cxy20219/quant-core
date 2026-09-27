package btengine

import (
	"fmt"
	"os"
	"runtime/pprof"
	"testing"
	"time"

	"quant-core/internal/lake"
	"quant-core/internal/schema"
)

// noopRunner 是不做任何事的策略执行器,用于隔离引擎侧耗时。
type noopRunner struct {
	bars int64
}

func (n *noopRunner) Initialize(host *Host, cfg Config) error {
	// 使用固定股票池(模拟 set_universe)
	host.Universe = benchUniverse()
	return nil
}
func (n *noopRunner) BeforeTradingStart() error { return nil }
func (n *noopRunner) HandleData(snapshot map[string]BarSnapshot) error {
	n.bars += int64(len(snapshot))
	return nil
}
func (n *noopRunner) RunDaily() error        { return nil }
func (n *noopRunner) AfterTradingEnd() error { return nil }
func (n *noopRunner) Close() error           { return nil }

// TestMinuteEngineBench 测量分钟引擎在大股票池下的耗时(不含 Python 子进程)。
// 默认跳过:设置 BENCH_MINUTE=1 时运行(需要本机数据湖 D:\quant-lake)。
func TestMinuteEngineBench(t *testing.T) {
	if os.Getenv("BENCH_MINUTE") == "" {
		t.Skip("设置 BENCH_MINUTE=1 运行(分钟引擎基准)")
	}
	if testing.Short() {
		t.Skip("short mode")
	}
	reg, err := schema.Load(`..\..\schemas\datasets.yaml`)
	if err != nil {
		t.Skip("registry unavailable:", err)
	}
	l := lake.New(`D:\quant-lake`, reg)
	universe := benchUniverse()
	if len(universe) == 0 {
		t.Skip("no universe")
	}
	start, _ := schema.ParseDate("20200102")
	end, _ := schema.ParseDate("20200131")
	cfg := Config{
		StrategyName: "bench",
		StartDate:    schema.TimeFromDays(start),
		EndDate:      schema.TimeFromDays(end),
		Frequency:    "1m",
		CapitalBase:  1_000_000,
		WarmupDays:   30,
	}
	engine := NewEngine(l, cfg)
	runner := &noopRunner{}
	if profile := os.Getenv("BENCH_PROFILE"); profile != "" {
		f, err := os.Create(profile)
		if err == nil {
			_ = pprof.StartCPUProfile(f)
			defer func() { pprof.StopCPUProfile(); _ = f.Close() }()
		}
	}
	t0 := time.Now()
	result, err := engine.Run(runner)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(t0)
	t.Logf("universe=%d bars=%d elapsed=%.2fs (%.3f ms/bar)",
		len(universe), runner.bars, elapsed.Seconds(),
		float64(elapsed.Milliseconds())/float64(runner.bars))
	if result != nil {
		t.Logf("nav rows=%d", len(result.Portfolio))
	}
}

// benchUniverse 读取 300 只股票代码(与 Python 基准一致)。
func benchUniverse() []string {
	// 直接内联前 300 只 2020 年日线代码,避免测试依赖外部文件
	raw := `000001.SZ,000002.SZ,000004.SZ,000006.SZ,000007.SZ,000008.SZ,000009.SZ,000010.SZ,000011.SZ,000012.SZ,
000014.SZ,000016.SZ,000017.SZ,000018.SZ,000019.SZ,000020.SZ,000021.SZ,000023.SZ,000025.SZ,000026.SZ,
000027.SZ,000028.SZ,000029.SZ,000030.SZ,000031.SZ,000032.SZ,000034.SZ,000035.SZ,000036.SZ,000037.SZ,
000039.SZ,000040.SZ,000042.SZ,000043.SZ,000045.SZ,000046.SZ,000048.SZ,000049.SZ,000050.SZ,000055.SZ,
000056.SZ,000058.SZ,000059.SZ,000060.SZ,000061.SZ,000062.SZ,000063.SZ,000065.SZ,000066.SZ,000068.SZ,
000069.SZ,000070.SZ,000078.SZ,000088.SZ,000089.SZ,000090.SZ,000096.SZ,000099.SZ,000100.SZ,000150.SZ,
000151.SZ,000153.SZ,000155.SZ,000156.SZ,000157.SZ,000158.SZ,000159.SZ,000166.SZ,000301.SZ,000333.SZ,
000338.SZ,000400.SZ,000401.SZ,000402.SZ,000403.SZ,000404.SZ,000407.SZ,000408.SZ,000410.SZ,000411.SZ,
000413.SZ,000415.SZ,000416.SZ,000417.SZ,000419.SZ,000420.SZ,000421.SZ,000422.SZ,000423.SZ,000425.SZ,
000426.SZ,000428.SZ,000429.SZ,000430.SZ,000488.SZ,000498.SZ,000501.SZ,000502.SZ,000503.SZ,000504.SZ,
000505.SZ,000506.SZ,000507.SZ,000509.SZ,000510.SZ,000513.SZ,000514.SZ,000516.SZ,000517.SZ,000518.SZ,
000519.SZ,000520.SZ,000521.SZ,000523.SZ,000524.SZ,000525.SZ,000526.SZ,000528.SZ,000529.SZ,000530.SZ,
000531.SZ,000532.SZ,000533.SZ,000534.SZ,000536.SZ,000537.SZ,000538.SZ,000539.SZ,000540.SZ,000541.SZ,
000543.SZ,000544.SZ,000545.SZ,000546.SZ,000547.SZ,000548.SZ,000550.SZ,000551.SZ,000552.SZ,000553.SZ,
000554.SZ,000555.SZ,000557.SZ,000558.SZ,000559.SZ,000560.SZ,000561.SZ,000563.SZ,000564.SZ,000565.SZ,
000566.SZ,000567.SZ,000568.SZ,000570.SZ,000571.SZ,000572.SZ,000573.SZ,000576.SZ,000581.SZ,000582.SZ,
000584.SZ,000585.SZ,000586.SZ,000587.SZ,000589.SZ,000590.SZ,000591.SZ,000592.SZ,000593.SZ,000595.SZ,
000596.SZ,000597.SZ,000598.SZ,000599.SZ,000600.SZ,000601.SZ,000603.SZ,000605.SZ,000606.SZ,000607.SZ,
000608.SZ,000609.SZ,000610.SZ,000611.SZ,000612.SZ,000613.SZ,000615.SZ,000616.SZ,000617.SZ,000619.SZ,
000620.SZ,000622.SZ,000623.SZ,000625.SZ,000626.SZ,000627.SZ,000628.SZ,000629.SZ,000630.SZ,000631.SZ,
000632.SZ,000633.SZ,000635.SZ,000636.SZ,000637.SZ,000638.SZ,000639.SZ,000650.SZ,000651.SZ,000652.SZ,
000655.SZ,000656.SZ,000657.SZ,000659.SZ,000661.SZ,000662.SZ,000663.SZ,000665.SZ,000666.SZ,000667.SZ,
000668.SZ,000669.SZ,000670.SZ,000671.SZ,000672.SZ,000673.SZ,000675.SZ,000676.SZ,000677.SZ,000678.SZ,
000679.SZ,000680.SZ,000681.SZ,000682.SZ,000683.SZ,000685.SZ,000686.SZ,000687.SZ,000688.SZ,000690.SZ,
000691.SZ,000692.SZ,000695.SZ,000697.SZ,000698.SZ,000700.SZ,000701.SZ,000702.SZ,000703.SZ,000705.SZ,
000707.SZ,000708.SZ,000709.SZ,000710.SZ,000711.SZ,000712.SZ,000713.SZ,000715.SZ,000716.SZ,000717.SZ,
000718.SZ,000719.SZ,000720.SZ,000721.SZ,000722.SZ,000723.SZ,000725.SZ,000726.SZ,000727.SZ,000728.SZ,
000729.SZ,000731.SZ,000732.SZ,000733.SZ,000735.SZ,000736.SZ,000737.SZ,000738.SZ,000739.SZ,000750.SZ,
000751.SZ,000752.SZ,000753.SZ,000755.SZ,000756.SZ,000757.SZ,000758.SZ,000759.SZ,000760.SZ,000761.SZ,
000762.SZ,000766.SZ,000767.SZ,000768.SZ,000776.SZ,000777.SZ,000778.SZ,000779.SZ,000780.SZ,000782.SZ`
	out := []string{}
	cur := ""
	for _, ch := range raw {
		if ch == ',' || ch == '\n' || ch == '\r' || ch == ' ' || ch == '\t' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(ch)
	}
	if cur != "" {
		out = append(out, cur)
	}
	// 转为内部代码(.SZ 保留)
	internal := make([]string, 0, len(out))
	for _, code := range out {
		internal = append(internal, code)
	}
	if len(internal) > 300 {
		internal = internal[:300]
	}
	return internal
}

var _ = fmt.Sprintf
