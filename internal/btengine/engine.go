package btengine

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"quant-core/internal/lake"
	"quant-core/internal/schema"
)

// DaysToTime 把 epoch days 转成 UTC 时间。
func DaysToTime(days int64) time.Time { return schema.TimeFromDays(days) }

// StrategyRunner 是策略执行器抽象(Python 子进程实现)。
//
// 引擎在每个生命周期节点调用对应方法;策略侧通过 Host 回调访问行情与下单。
type StrategyRunner interface {
	// Initialize 加载并运行策略的 initialize。
	Initialize(host *Host, config Config) error
	// BeforeTradingStart 运行 before_trading_start。
	BeforeTradingStart() error
	// HandleData 运行 handle_data(传入当前 Bar 快照)。
	HandleData(snapshot map[string]BarSnapshot) error
	// RunDaily 运行 run_daily 注册的回调(日线在 handle_data 后触发)。
	RunDaily() error
	// AfterTradingEnd 运行 after_trading_end。
	AfterTradingEnd() error
	// Close 结束策略进程。
	Close() error
}

// BarSnapshot 是传给策略的当前 Bar。
type BarSnapshot struct {
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume float64 `json:"volume"`
	Amount float64 `json:"amount"`
}

// Host 暴露给策略侧(通过 RPC)的引擎能力。
type Host struct {
	Engine   *Engine
	Portal   *DataPortal
	Broker   *Broker
	Portfolio *Portfolio
	Config   Config
	Universe []string
	Logs     *[]LogRecord
	// 当前状态
	CurrentDay   int64
	CurrentBars  map[string]Bar
	PreviousDay  int64
}

// Engine 是回测引擎。
type Engine struct {
	Lake     *lake.Lake
	Config   Config
	Portal   *DataPortal
	Broker   *Broker
	Portfolio *Portfolio
	Logs     []LogRecord
	Nav      []NavRow
	runner   StrategyRunner
	days     []int64
	startTs  time.Time
}

// NewEngine 创建引擎。
func NewEngine(l *lake.Lake, cfg Config) *Engine {
	return &Engine{Lake: l, Config: cfg}
}

// Run 执行回测。
func (e *Engine) Run(runner StrategyRunner) (*Result, error) {
	e.startTs = time.Now()
	e.runner = runner
	startDays, err := schema.ParseDate(e.Config.StartDate.Format("20060102"))
	if err != nil {
		return nil, err
	}
	endDays, err := schema.ParseDate(e.Config.EndDate.Format("20060102"))
	if err != nil {
		return nil, err
	}
	warmup := int64(e.Config.WarmupDays)
	if warmup <= 0 {
		warmup = 365
	}
	e.Portal = NewDataPortal(e.Lake, startDays, endDays, warmup)
	days := e.Portal.TradingDaysInRange(startDays, endDays)

	if len(days) == 0 {
		return nil, fmt.Errorf("回测区间内无交易日数据")
	}
	e.days = days

	e.Portfolio = &Portfolio{
		Cash:      e.Config.CapitalBase,
		Positions: map[string]*Position{},
	}
	e.Portfolio.PortfolioValue = e.Portfolio.Cash
	e.Broker = NewBroker(e.Portfolio)

	host := &Host{
		Engine:    e,
		Portal:    e.Portal,
		Broker:    e.Broker,
		Portfolio: e.Portfolio,
		Config:    e.Config,
		Logs:      &e.Logs,
	}
	if err := e.runner.Initialize(host, e.Config); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	// 股票池由策略 initialize 通过 set_universe 设置;此处按需加载
	if len(host.Universe) > 0 {
		if err := e.Portal.EnsureDaily(host.Universe); err != nil {
			return nil, err
		}
		e.Portal.Adfactors(host.Universe)
	}

	prevDay := int64(0)
	for _, day := range days {
		host.PreviousDay = prevDay
		host.CurrentDay = day
		e.Broker.StartDay(day)

		// 每个交易日开始前,确保当前股票池数据已加载(策略可能在盘前切换股票池)
		if len(host.Universe) > 0 {
			if err := e.Portal.EnsureDaily(host.Universe); err != nil {
				return nil, err
			}
		}
		if err := e.runner.BeforeTradingStart(); err != nil {
			return nil, fmt.Errorf("%s before_trading_start: %w", dayString(day), err)
		}
		// 盘前可能更新股票池
		if len(host.Universe) > 0 {
			if err := e.Portal.EnsureDaily(host.Universe); err != nil {
				return nil, err
			}
			e.Portal.Adfactors(host.Universe)
		}

		bars := e.Portal.DailyBars(day)
		visible := make(map[string]Bar)
		for _, sec := range host.Universe {
			if bar, ok := bars[sec]; ok {
				visible[sec] = bar
			}
		}
		e.Broker.SetCurrentPrices(visible)
		e.markToMarket(visible)
		host.CurrentBars = visible

		snapshot := make(map[string]BarSnapshot, len(visible))
		for sec, bar := range visible {
			snapshot[sec] = BarSnapshot{
				Open: bar.Open, High: bar.High, Low: bar.Low,
				Close: bar.Close, Volume: bar.Volume, Amount: bar.Amount,
			}
		}
		if err := e.runner.HandleData(snapshot); err != nil {
			return nil, fmt.Errorf("%s handle_data: %w", dayString(day), err)
		}
		if err := e.runner.RunDaily(); err != nil {
			return nil, fmt.Errorf("%s run_daily: %w", dayString(day), err)
		}
		// 挂单撮合与重新估值
		e.Broker.MatchPending(visible)
		e.markToMarket(visible)
		if err := e.runner.AfterTradingEnd(); err != nil {
			return nil, fmt.Errorf("%s after_trading_end: %w", dayString(day), err)
		}
		e.Broker.ExpirePending()

		e.Nav = append(e.Nav, NavRow{
			Date:           dayString(day),
			PortfolioValue: e.Portfolio.PortfolioValue,
			Cash:           e.Portfolio.Cash,
			PositionsValue: e.Portfolio.PositionsValue,
			Returns:        e.Portfolio.Returns,
		})
		prevDay = day
	}
	if err := e.runner.Close(); err != nil {
		return nil, err
	}
	return e.buildResult(), nil
}

// markToMarket 用当前 Bar 重新估值。
func (e *Engine) markToMarket(bars map[string]Bar) {
	positionsValue := 0.0
	for sec, pos := range e.Portfolio.Positions {
		if bar, ok := bars[sec]; ok {
			pos.LastSalePrice = bar.Close
		}
		positionsValue += float64(pos.Amount) * pos.LastSalePrice
	}
	e.Portfolio.PositionsValue = positionsValue
	e.Portfolio.PortfolioValue = e.Portfolio.Cash + positionsValue
	if e.Config.CapitalBase > 0 {
		e.Portfolio.Returns = e.Portfolio.PortfolioValue/e.Config.CapitalBase - 1
	}
}

func (e *Engine) buildResult() *Result {
	result := &Result{
		StrategyName: e.Config.StrategyName,
		Portfolio:    e.Nav,
		Orders:       e.Broker.Orders(),
		Trades:       e.Broker.Trades(),
		Logs:         e.Logs,
		Params:       e.Config.Params,
	}
	if len(e.Nav) > 0 {
		result.StartDate = e.Nav[0].Date
		result.EndDate = e.Nav[len(e.Nav)-1].Date
		result.Summary = Summary{
			InitialValue:  e.Config.CapitalBase,
			FinalValue:    e.Nav[len(e.Nav)-1].PortfolioValue,
			TotalReturn:   e.Nav[len(e.Nav)-1].PortfolioValue/e.Config.CapitalBase - 1,
			TradingDays:   len(e.Nav),
			OrderCount:    len(result.Orders),
			TradeCount:    len(result.Trades),
			ElapsedSecond: time.Since(e.startTs).Seconds(),
		}
		result.Analytics = computeAnalytics(e.Nav, e.Config.CapitalBase, result.Trades, e.BenchmarkSeries())
	}
	return result
}

// BenchmarkSeries 返回基准(可选)的日收益对齐序列:日期 → 收盘。
func (e *Engine) BenchmarkSeries() []float64 {
	if e.Config.Benchmark == "" {
		return nil
	}
	if err := e.Portal.EnsureDaily([]string{e.Config.Benchmark}); err != nil {
		return nil
	}
	out := make([]float64, len(e.Nav))
	for i, row := range e.Nav {
		days, _ := schema.ParseDate(strings.ReplaceAll(row.Date, "-", ""))
		out[i] = e.Portal.CloseAt(e.Config.Benchmark, days)
	}
	return out
}

// computeAnalytics 计算指标(与数据服务 web.py 口径一致的最小集)。
func computeAnalytics(nav []NavRow, capital float64, trades []*Trade, benchmark []float64) map[string]any {
	out := map[string]any{}
	if len(nav) < 2 {
		return out
	}
	values := make([]float64, len(nav))
	for i, row := range nav {
		values[i] = row.PortfolioValue
	}
	intervals := len(values) - 1
	totalReturn := values[len(values)-1]/values[0] - 1
	annualized := math.Pow(1+totalReturn, 252.0/float64(intervals)) - 1

	// 日收益
	returns := make([]float64, 0, intervals)
	for i := 1; i < len(values); i++ {
		if values[i-1] > 0 {
			returns = append(returns, values[i]/values[i-1]-1)
		}
	}
	mean := avg(returns)
	std := stddev(returns)
	sharpe := 0.0
	if std > 0 {
		sharpe = mean / std * math.Sqrt(252)
	}
	// 下行波动与索提诺
	downside := []float64{}
	for _, r := range returns {
		if r < 0 {
			downside = append(downside, r*r)
		}
	}
	downDev := 0.0
	if len(downside) > 0 {
		downDev = math.Sqrt(avg(downside))
	}
	sortino := 0.0
	if downDev > 0 {
		sortino = mean / downDev * math.Sqrt(252)
	}
	// 最大回撤
	peak := values[0]
	maxDD := 0.0
	ddStart, ddEnd := nav[0].Date, nav[0].Date
	peakDate := nav[0].Date
	for i, v := range values {
		if v > peak {
			peak = v
			peakDate = nav[i].Date
		}
		dd := v/peak - 1
		if dd < maxDD {
			maxDD = dd
			ddStart = peakDate
			ddEnd = nav[i].Date
		}
	}
	calmar := 0.0
	if maxDD < 0 {
		calmar = annualized / math.Abs(maxDD)
	}
	out["annualized_return"] = annualized
	out["volatility"] = std * math.Sqrt(252)
	out["sharpe_ratio"] = sharpe
	out["sortino_ratio"] = sortino
	out["max_drawdown"] = maxDD
	out["max_drawdown_start"] = ddStart
	out["max_drawdown_end"] = ddEnd
	out["calmar_ratio"] = calmar
	out["daily_win_rate"] = winRate(returns)
	// 交易统计
	tradeStats := tradeAnalytics(trades)
	for k, v := range tradeStats {
		out[k] = v
	}
	// 基准
	if len(benchmark) == len(values) && benchmark[0] > 0 {
		bReturn := benchmark[len(benchmark)-1]/benchmark[0] - 1
		out["benchmark_total_return"] = bReturn
		out["excess_return"] = totalReturn - bReturn
		bReturns := make([]float64, 0, len(benchmark)-1)
		for i := 1; i < len(benchmark); i++ {
			if benchmark[i-1] > 0 {
				bReturns = append(bReturns, benchmark[i]/benchmark[i-1]-1)
			}
		}
		if len(bReturns) == len(returns) {
			out["benchmark_annualized_return"] = math.Pow(1+bReturn, 252.0/float64(len(bReturns))) - 1
			active := make([]float64, len(returns))
			for i := range returns {
				active[i] = returns[i] - bReturns[i]
			}
			aStd := stddev(active)
			if aStd > 0 {
				out["information_ratio"] = avg(active) / aStd * math.Sqrt(252)
			}
			bVar := variance(bReturns)
			if bVar > 0 {
				beta := covariance(returns, bReturns) / bVar
				out["beta"] = beta
				out["alpha"] = (mean - beta*avg(bReturns)) * 252
			}
		}
	}
	return out
}

func tradeAnalytics(trades []*Trade) map[string]any {
	type lot struct {
		amount int64
		price  float64
	}
	lots := map[string][]lot{}
	wins, losses := 0, 0
	profit, loss := 0.0, 0.0
	for _, tr := range trades {
		if tr.Side == "buy" {
			lots[tr.Security] = append(lots[tr.Security], lot{tr.Amount, tr.Price})
			continue
		}
		remaining := tr.Amount
		for remaining > 0 && len(lots[tr.Security]) > 0 {
			l := &lots[tr.Security][0]
			matched := remaining
			if l.amount < matched {
				matched = l.amount
			}
			pnl := (tr.Price - l.price) * float64(matched)
			if pnl > 0 {
				wins++
				profit += pnl
			} else if pnl < 0 {
				losses++
				loss -= pnl
			}
			l.amount -= matched
			remaining -= matched
			if l.amount == 0 {
				lots[tr.Security] = lots[tr.Security][1:]
			}
		}
	}
	closed := wins + losses
	wr := 0.0
	if closed > 0 {
		wr = float64(wins) / float64(closed)
	}
	plr := 0.0
	if wins > 0 && losses > 0 {
		plr = (profit / float64(wins)) / (loss / float64(losses))
	}
	return map[string]any{
		"win_rate":         wr,
		"profit_loss_ratio": plr,
		"winning_count":    wins,
		"losing_count":     losses,
	}
}

func avg(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func stddev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := avg(xs)
	s := 0.0
	for _, x := range xs {
		s += (x - m) * (x - m)
	}
	return math.Sqrt(s / float64(len(xs)))
}

func variance(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := avg(xs)
	s := 0.0
	for _, x := range xs {
		s += (x - m) * (x - m)
	}
	return s / float64(len(xs))
}

func covariance(xs, ys []float64) float64 {
	n := len(xs)
	if n < 2 || len(ys) != n {
		return 0
	}
	mx, my := avg(xs), avg(ys)
	s := 0.0
	for i := 0; i < n; i++ {
		s += (xs[i] - mx) * (ys[i] - my)
	}
	return s / float64(n)
}

func winRate(returns []float64) float64 {
	if len(returns) == 0 {
		return 0
	}
	wins := 0
	for _, r := range returns {
		if r > 0 {
			wins++
		}
	}
	return float64(wins) / float64(len(returns))
}

// MarshalJSON 便捷序列化(供 RPC 使用)。
func (r *Result) MarshalJSON() ([]byte, error) {
	type alias Result
	return json.Marshal((*alias)(r))
}

var _ = sort.Ints
