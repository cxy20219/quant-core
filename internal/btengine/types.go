// Package btengine 实现回测引擎核心:日线时钟、账户与持仓、撮合与费用、结果汇总。
//
// 语义对齐 quant-data 的 quantbt(PTrade 兼容运行时),便于逐项对拍:
//   - 日线:08:30 公司行动 → before_trading_start;15:00 handle_data → run_daily →
//     挂单撮合 → 重新估值;15:30 after_trading_end → 挂单失效
//   - 市价单按当前 Bar 收盘价成交;限价单当收盘价满足限价时成交
//   - 滑点按方向取半(price * (1 ± slippage/2));费用 = 佣金(max(万三, 5) + 经手费 0.0000487)+ 卖出印花税 0.001
package btengine

import (
	"time"
)

// Config 是回测配置。
type Config struct {
	StrategyName string
	StartDate    time.Time
	EndDate      time.Time
	Frequency    string // "1d"(v1 仅日线)
	CapitalBase  float64
	Benchmark    string
	WarmupDays   int
	Params       map[string]any
}

// Commission 是费用参数(与 quantbt 的 Commission 对齐)。
type Commission struct {
	Cost         float64 // 佣金比例,默认 0.0003
	MinTradeCost float64 // 最低佣金,默认 5
	Tax          float64 // 印花税(仅卖出),默认 0.001
}

// DefaultCommission 返回默认费用。
func DefaultCommission() Commission {
	return Commission{Cost: 0.0003, MinTradeCost: 5.0, Tax: 0.001}
}

// Slippage 是滑点参数。Fixed 为 true 时按固定价差(±value/2),否则按比例(±value/2)。
type Slippage struct {
	Value float64
	Fixed bool
}

// Bar 是单只证券的行情切片。
type Bar struct {
	Security string
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   float64
	Amount   float64
}

// Position 是持仓。
type Position struct {
	Security      string
	Amount        int64   // 总持仓(股)
	EnableAmount  int64   // 可卖数量(T+1:当日买入不计入)
	TodayAmount   int64   // 当日买入数量(次日开盘转入可卖)
	LastSalePrice float64 // 最新价
	CostBasis     float64 // 成本价(含费用分摊)
}

// Order 是委托。
type Order struct {
	ID         string  `json:"id"`
	Security   string  `json:"security"`
	Amount     int64   `json:"amount"`      // 正买负卖
	Filled     int64   `json:"filled"`      // 已成交数量(带方向)
	Limit      float64 `json:"limit_price"` // 限价(0 表示市价)
	Status     string  `json:"status"`      // 2=挂单 6=部分成交 8=全部成交 9=已撤
	CreatedAt  string  `json:"created_at"`
	FilledAt   string  `json:"filled_at,omitempty"`
	FilledAmt  int64   `json:"filled_amount"`
	FilledPx   float64 `json:"filled_price"`
	OrderType  string  `json:"order_type"` // market / limit
	OrigAmount int64   `json:"orig_amount"`

	// 挂单冻结(仅未成交限价单):
	ReservedCash float64 `json:"-"` // 已冻结现金 = 数量×限价×(1±滑点/2) + 费用
	ReservedValue float64 `json:"-"` // 冻结时的成交估值(数量×限价×(1±滑点/2))
	ReleasedCash float64 `json:"-"` // 到期释放现金 = 数量×限价
	FrozenAmount int64   `json:"-"` // 已冻结股数(卖出挂单)
}

// Origin 返回原始委托数量(正买负卖)。
func (o *Order) Origin() int64 { return o.OrigAmount }

// Trade 是成交。
type Trade struct {
	TradeID   string  `json:"trade_id"`
	OrderID   string  `json:"order_id"`
	Security  string  `json:"security"`
	Side      string  `json:"side"` // buy / sell
	Amount    int64   `json:"amount"`
	Price     float64 `json:"price"`
	Value     float64 `json:"value"`
	TradeTime string  `json:"trade_time"`
}

// Portfolio 是账户状态。
type Portfolio struct {
	Cash           float64
	PositionsValue float64
	PortfolioValue float64
	Returns        float64
	Positions      map[string]*Position
}

// LogRecord 是一条策略日志。
type LogRecord struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Message   string `json:"message"`
}

// NavRow 是日终净值记录。
type NavRow struct {
	Date           string  `json:"date"`
	PortfolioValue float64 `json:"portfolio_value"`
	Cash           float64 `json:"cash"`
	PositionsValue float64 `json:"positions_value"`
	Returns        float64 `json:"returns"`
}

// Result 是回测结果。
type Result struct {
	StrategyName string       `json:"strategy_name"`
	StartDate    string       `json:"start_date"`
	EndDate      string       `json:"end_date"`
	Portfolio    []NavRow     `json:"portfolio"`
	Orders       []*Order     `json:"orders"`
	Cancelled    []*Order     `json:"cancelled_orders,omitempty"` // 当日过期撤销的挂单(PTrade 订单列表不含它们)
	Trades       []*Trade     `json:"trades"`
	Logs         []LogRecord  `json:"logs"`
	Summary      Summary      `json:"summary"`
	Analytics    map[string]any `json:"analytics"`
	Params       map[string]any `json:"params,omitempty"`
}

// Summary 是结果概要。
type Summary struct {
	InitialValue  float64 `json:"initial_value"`
	FinalValue    float64 `json:"final_value"`
	TotalReturn   float64 `json:"total_return"`
	TradingDays   int     `json:"trading_days"`
	OrderCount    int     `json:"order_count"`
	TradeCount    int     `json:"trade_count"`
	ElapsedSecond float64 `json:"elapsed_seconds"`
}
