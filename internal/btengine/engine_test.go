package btengine

import (
	"path/filepath"
	"testing"

	"quant-core/internal/lake"
	"quant-core/internal/schema"
)

// writeTestLake 写一份最小日线数据(1 只证券,10 个交易日)。
func writeTestLake(t *testing.T, closes []float64) *lake.Lake {
	t.Helper()
	reg, err := schema.Load(filepath.Join("..", "..", "schemas", "datasets.yaml"))
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	l := lake.New(t.TempDir(), reg)
	ds, _ := reg.Get("bars_daily")
	dir := filepath.Join(l.Dir(ds), "year=2024")
	pw, err := lake.NewPartWriter(ds, dir)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	day, _ := schema.ParseDate("20240102")
	for i, close := range closes {
		row := make([]schema.Value, len(ds.Fields))
		for j := range row {
			row[j] = schema.NullValue()
		}
		set := func(name string, v schema.Value) {
			idx, _ := ds.FieldIndex(name)
			row[idx] = v
		}
		set("ts_code", schema.Str("600000.SH"))
		set("trade_date", schema.Date(day+int64(i)))
		set("open", schema.Float(close))
		set("high", schema.Float(close))
		set("low", schema.Float(close))
		set("close", schema.Float(close))
		set("vol", schema.Float(1_000_000))
		set("amount", schema.Float(close * 1_000_000))
		if err := pw.WriteRow(row); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if _, err := pw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return l
}

// fakeRunner 是测试用的策略执行器。
type fakeRunner struct {
	host     *Host
	buyDay   int
	sellDay  int
	orders   int
	seenDays int
}

func (f *fakeRunner) Initialize(host *Host, cfg Config) error {
	f.host = host
	host.Universe = []string{"600000.SH"}
	return nil
}
func (f *fakeRunner) BeforeTradingStart() error { return nil }
func (f *fakeRunner) RunDaily() error           { return nil }
func (f *fakeRunner) AfterTradingEnd() error    { return nil }
func (f *fakeRunner) Close() error              { return nil }

func (f *fakeRunner) HandleData(snapshot map[string]BarSnapshot) error {
	f.seenDays++
	bar, ok := f.host.CurrentBars["600000.SH"]
	if !ok {
		return nil
	}
	switch f.seenDays {
	case 2:
		f.host.Broker.Order("600000.SH", 1000, 0, bar)
		f.orders++
	case 4:
		f.host.Broker.Order("600000.SH", -1000, 0, bar)
		f.orders++
	}
	return nil
}

func TestEngineBasicTrading(t *testing.T) {
	closes := []float64{10, 10, 11, 12, 13, 12, 11, 12, 13, 14}
	l := writeTestLake(t, closes)
	start, _ := schema.ParseDate("20240102")
	end, _ := schema.ParseDate("20240111")
	cfg := Config{
		StrategyName: "test",
		StartDate:    schema.TimeFromDays(start),
		EndDate:      schema.TimeFromDays(end),
		Frequency:    "1d",
		CapitalBase:  100_000,
		WarmupDays:   0,
	}
	engine := NewEngine(l, cfg)
	runner := &fakeRunner{}
	result, err := engine.Run(runner)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if runner.seenDays != 10 {
		t.Fatalf("handle_data 调用 %d 次,期望 10", runner.seenDays)
	}
	if len(result.Trades) != 2 {
		t.Fatalf("成交 %d 笔,期望 2", len(result.Trades))
	}
	buy, sell := result.Trades[0], result.Trades[1]
	if buy.Side != "buy" || buy.Amount != 1000 {
		t.Errorf("买单异常: %+v", buy)
	}
	if sell.Side != "sell" || sell.Amount != 1000 {
		t.Errorf("卖单异常: %+v", sell)
	}
	// 第 2 个交易日收盘 10 买入,第 4 个交易日收盘 13 卖出:
	// 毛利 3000,减去双边费用后应不足 3000
	pnl := (sell.Price - buy.Price) * 1000
	if pnl <= 0 || pnl > 3000 {
		t.Errorf("价差收益异常: %.2f", pnl)
	}
	final := result.Summary.FinalValue
	if final <= 100_000 {
		t.Errorf("期末应盈利: %.2f", final)
	}
	if result.Summary.FinalValue > 100_000+3000 {
		t.Errorf("期末超过理论上限: %.2f", final)
	}
	// 委托状态:全部成交
	for _, o := range result.Orders {
		if o.Status != "8" {
			t.Errorf("委托状态应为 8(全成): %+v", o)
		}
	}
	// 净值序列长度 = 交易日数
	if len(result.Portfolio) != 10 {
		t.Errorf("净值序列 %d 条,期望 10", len(result.Portfolio))
	}
	// 期末无持仓,现金应等于组合价值
	last := result.Portfolio[len(result.Portfolio)-1]
	if last.PositionsValue != 0 {
		t.Errorf("期末应无持仓,positions_value=%.2f", last.PositionsValue)
	}
	if diff := last.PortfolioValue - last.Cash; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("期末现金与组合价值不一致: %.6f vs %.6f", last.Cash, last.PortfolioValue)
	}
}

func TestEngineLimitOrderPending(t *testing.T) {
	closes := []float64{10, 10, 9, 8, 11, 12}
	l := writeTestLake(t, closes)
	start, _ := schema.ParseDate("20240102")
	end, _ := schema.ParseDate("20240107")
	cfg := Config{
		StrategyName: "limit-test",
		StartDate:    schema.TimeFromDays(start),
		EndDate:      schema.TimeFromDays(end),
		Frequency:    "1d",
		CapitalBase:  100_000,
	}
	engine := NewEngine(l, cfg)
	runner := &limitRunner{limit: 8.5}
	result, err := engine.Run(runner)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// 日线语义:未成交限价单在当日 after_trading_end 后撤销(PTrade 语义),
	// 因此本用例不应有成交,且委托状态应为 9(已撤)。
	if len(result.Trades) != 0 {
		t.Fatalf("日级挂单不应在后续交易日成交,实际成交 %d 笔", len(result.Trades))
	}
	if len(result.Orders) != 1 || result.Orders[0].Status != "9" {
		t.Fatalf("挂单应被撤销(status=9): %+v", result.Orders)
	}
	// 撤单后资金应完整保留(无冻结损失)
	last := result.Portfolio[len(result.Portfolio)-1]
	if diff := last.Cash - cfg.CapitalBase; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("撤单后现金应等于初始资金: %.6f vs %.6f", last.Cash, cfg.CapitalBase)
	}
}

func TestEngineLimitOrderFillsIntraday(t *testing.T) {
	// 限价单在当日即可成交:第 2 天收盘 9,限价 9.5 → 立即成交
	closes := []float64{10, 9, 9, 9, 9, 9}
	l := writeTestLake(t, closes)
	start, _ := schema.ParseDate("20240102")
	end, _ := schema.ParseDate("20240107")
	cfg := Config{
		StrategyName: "limit-fill",
		StartDate:    schema.TimeFromDays(start),
		EndDate:      schema.TimeFromDays(end),
		Frequency:    "1d",
		CapitalBase:  100_000,
	}
	engine := NewEngine(l, cfg)
	runner := &limitRunner{limit: 9.5}
	result, err := engine.Run(runner)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(result.Trades) != 1 {
		t.Fatalf("限价单应立即成交,实际 %d 笔", len(result.Trades))
	}
	if price := result.Trades[0].Price; price > 9.5 {
		t.Errorf("成交价超过限价: %.2f", price)
	}
}

type limitRunner struct {
	host  *Host
	limit float64
	day   int
}

func (f *limitRunner) Initialize(host *Host, cfg Config) error {
	f.host = host
	host.Universe = []string{"600000.SH"}
	return nil
}
func (f *limitRunner) BeforeTradingStart() error { return nil }
func (f *limitRunner) RunDaily() error           { return nil }
func (f *limitRunner) AfterTradingEnd() error    { return nil }
func (f *limitRunner) Close() error              { return nil }
func (f *limitRunner) HandleData(snapshot map[string]BarSnapshot) error {
	f.day++
	if f.day == 2 {
		bar := f.host.CurrentBars["600000.SH"]
		f.host.Broker.Order("600000.SH", 1000, f.limit, bar)
	}
	return nil
}
