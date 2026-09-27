package btengine

import (
	"fmt"
	"sort"
)

// HandlingFeeRate 是经手费率(与 quantbt 对齐)。
const HandlingFeeRate = 0.0000487

// Broker 负责订单、撮合、费用与持仓变动。
//
// 语义与 quantbt(PTrade 兼容)逐项对齐,要点:
//   - 市价单/可成交限价单:先按资金上限取整,再按当 Bar 成交量上限截断;
//     被成交量截断时记部分成交(status 6)并按"原始下单量"计费,剩余自动撤销;
//   - 不可成交限价单:挂单并冻结(买入冻结现金、卖出冻结可卖股数),
//     当日 after_trading_end 后撤销,买入仅释放 数量×限价(滑点与费用不退);
//   - 触发成交的挂单按当前 Bar 收盘价(含滑点)成交;
//   - 卖出以可卖数量为上限(T+1)。
type Broker struct {
	portfolio     *Portfolio
	commission    Commission
	slippage      Slippage
	volumeRatio   float64
	limitMode     string // LIMIT / UNLIMITED
	orders        []*Order
	expired       []*Order // 当日过期撤销的挂单(PTrade 订单列表不含它们)
	orderIndex    map[string]*Order
	pending       []*Order
	trades        []*Trade
	tradeSeq      int
	currentDay    int64
	currentCloses map[string]float64
	volumeUsed    map[string]float64
}

// NewBroker 创建撮合器。
func NewBroker(p *Portfolio) *Broker {
	return &Broker{
		portfolio:  p,
		commission: DefaultCommission(),
		volumeRatio: 0.25,
		limitMode:  "LIMIT",
		orderIndex: map[string]*Order{},
		volumeUsed: map[string]float64{},
	}
}

// SetCommission 设置费用。
func (b *Broker) SetCommission(c Commission) { b.commission = c }

// SetSlippage 设置滑点。
func (b *Broker) SetSlippage(s Slippage) { b.slippage = s }

// SetVolumeRatio 设置成交量比例。
func (b *Broker) SetVolumeRatio(r float64) { b.volumeRatio = r }

// SetLimitMode 设置限价模式。
func (b *Broker) SetLimitMode(m string) { b.limitMode = m }

// StartDay 开始新的交易日:前一日持仓全部转为可卖(T+1),清空当日成交量预算。
func (b *Broker) StartDay(day int64) {
	b.currentDay = day
	b.volumeUsed = map[string]float64{}
	for _, pos := range b.portfolio.Positions {
		pos.EnableAmount = pos.Amount
		pos.TodayAmount = 0
	}
}

// SetCurrentPrices 记录当前 Bar 的收盘价(用于撮合与估值)。
func (b *Broker) SetCurrentPrices(bars map[string]Bar) {
	b.currentCloses = map[string]float64{}
	for sec, bar := range bars {
		b.currentCloses[sec] = bar.Close
	}
}

// Order 提交订单。amount 正买负卖;limitPrice<=0 表示市价。
func (b *Broker) Order(sec string, amount int64, limitPrice float64, bar Bar) *Order {
	amount = b.roundAmount(sec, amount)
	if amount == 0 {
		return nil
	}
	order := &Order{
		ID:         b.nextOrderID(),
		Security:   sec,
		Amount:     amount,
		OrigAmount: amount,
		Limit:      limitPrice,
		Status:     "2",
		CreatedAt:  dayString(b.currentDay),
	}

	// ── 挂单路径:限价且当前价不可成交 ──
	if limitPrice > 0 && !b.marketable(amount, limitPrice, bar.Close) {
		order.OrderType = "limit"
		reservedPrice := b.executionPrice(limitPrice, amount)
		if amount > 0 {
			amount = b.capBuyAmount(sec, amount, reservedPrice)
			if amount == 0 {
				return nil
			}
			value := float64(amount) * reservedPrice
			order.Amount, order.OrigAmount = amount, amount
			order.ReservedCash = value + b.fees(value, false)
			order.ReservedValue = value
			order.ReleasedCash = float64(amount) * limitPrice
			b.portfolio.Cash -= order.ReservedCash
		} else {
			pos := b.GetPosition(sec)
			amount = -min64(abs64(amount), pos.EnableAmount)
			if amount == 0 {
				return nil
			}
			order.Amount, order.OrigAmount = amount, amount
			order.FrozenAmount = abs64(amount)
			pos.EnableAmount -= order.FrozenAmount
		}
		b.pending = append(b.pending, order)
		b.register(order)
		return order
	}

	// ── 成交路径:市价 或 可成交限价(按当前 Bar 收盘价成交) ──
	order.OrderType = "market"
	if limitPrice > 0 {
		order.OrderType = "limit"
	}
	base := bar.Close
	sidePrice := b.executionPrice(base, amount)
	if amount > 0 {
		amount = b.capBuyAmount(sec, amount, sidePrice)
	} else {
		pos := b.GetPosition(sec)
		amount = -min64(abs64(amount), pos.EnableAmount)
	}
	if amount == 0 {
		return nil
	}
	beforeVolume := amount
	amount = b.capVolume(sec, amount, bar.Volume)
	autoCancelled := amount != beforeVolume

	if amount == 0 {
		// 完全被成交量截断:记一笔零成交委托(status 6)并按原始数量计费
		order.Amount, order.OrigAmount = beforeVolume, beforeVolume
		order.Status = "6"
		if beforeVolume > 0 {
			b.portfolio.Cash -= b.fees(float64(beforeVolume)*sidePrice, false)
		}
		b.register(order)
		return order
	}

	cashValue := -1.0
	cashFeeValue := -1.0
	if amount > 0 && amount < beforeVolume {
		// 部分成交:费用按原始下单量计提,滑点差额一并冻结(与 quantbt 一致)
		cashValue = float64(amount)*base + float64(beforeVolume-amount)*(sidePrice-base)
		if limitPrice > 0 {
			cashValue -= float64(beforeVolume-amount) * (limitPrice - base)
		}
		cashFeeValue = float64(beforeVolume) * sidePrice
	}
	order.Amount, order.OrigAmount = beforeVolume, beforeVolume
	order.Filled = amount
	order.FilledPx = sidePrice
	order.Status = "6"
	if !autoCancelled {
		order.Amount, order.OrigAmount = amount, amount
		order.Status = "8"
	}
	b.applyFill(sec, amount, sidePrice, cashValue, cashFeeValue)
	// PTrade:现金按含滑点成交价扣减,但持仓按当前 Bar 收盘价估值
	if pos, ok := b.portfolio.Positions[sec]; ok {
		pos.LastSalePrice = bar.Close
	}
	b.register(order)
	return order
}

// capBuyAmount 按可用现金(含费用)把买入数量向下取整到可负担的手数。
func (b *Broker) capBuyAmount(sec string, amount int64, price float64) int64 {
	lot := int64(lotSize(sec))
	for amount > 0 {
		value := float64(amount) * price
		if value+b.fees(value, false) <= b.portfolio.Cash+1e-9 {
			return amount
		}
		amount -= lot
	}
	return 0
}

// MatchPending 撮合挂单(每个 Bar 结束后调用)。
func (b *Broker) MatchPending(bars map[string]Bar) {
	remaining := b.pending[:0]
	for _, order := range b.pending {
		bar, ok := bars[order.Security]
		if !ok {
			remaining = append(remaining, order) // 停牌或无行情,继续挂单
			continue
		}
		if !b.marketable(order.Amount, order.Limit, bar.Close) {
			remaining = append(remaining, order)
			continue
		}
		fillAmount := b.capVolume(order.Security, order.Amount-order.Filled, bar.Volume)
		if fillAmount == 0 {
			remaining = append(remaining, order)
			continue
		}
		if order.ReservedCash > 0 {
			b.portfolio.Cash += order.ReservedCash
			order.ReservedCash = 0
		}
		fillPrice := b.executionPrice(bar.Close, order.Amount)
		partial := fillAmount != order.Amount-order.Filled
		cashValue := -1.0
		cashFeeValue := -1.0
		if partial && order.ReservedValue > 0 {
			unfilled := order.Amount - order.Filled - fillAmount
			cashValue = order.ReservedValue - float64(unfilled)*order.Limit
			cashFeeValue = order.ReservedValue
		}
		if partial && order.ReservedValue == 0 {
			cashValue = float64(fillAmount) * order.Limit
			cashFeeValue = float64(order.Amount) * order.Limit
		}
		b.applyFill(order.Security, fillAmount, fillPrice, cashValue, cashFeeValue)
		order.Filled += fillAmount
		order.FilledPx = fillPrice
		order.FilledAt = dayString(b.currentDay)
		if order.Filled == order.Amount && !partial {
			order.Status = "8"
		} else {
			order.Status = "6"
		}
		if order.Filled == order.Amount {
			// 全部成交,移出挂单
			continue
		}
		remaining = append(remaining, order)
	}
	b.pending = remaining
}

// ExpirePending 当日结束使未成交挂单失效(PTrade 语义):
// 买入仅释放 数量×限价(滑点与费用不退),卖出释放冻结股数;订单从有效列表移除并标记状态 9。
func (b *Broker) ExpirePending() {
	for _, o := range b.pending {
		if o.ReleasedCash > 0 {
			b.portfolio.Cash += o.ReleasedCash
			o.ReleasedCash = 0
		}
		if o.FrozenAmount > 0 {
			pos := b.ensurePosition(o.Security)
			pos.EnableAmount = min64(pos.Amount, pos.EnableAmount+o.FrozenAmount)
			o.FrozenAmount = 0
		}
		o.Status = "9"
		b.expired = append(b.expired, o)
		delete(b.orderIndex, o.ID)
	}
	b.pending = nil
}

// Positions 返回持仓表。
func (b *Broker) Positions() map[string]*Position { return b.portfolio.Positions }

// Orders 返回有效委托(不含当日过期撤销的挂单,与 PTrade 一致)。
func (b *Broker) Orders() []*Order {
	if len(b.expired) == 0 {
		return b.orders
	}
	removed := make(map[string]bool, len(b.expired))
	for _, o := range b.expired {
		removed[o.ID] = true
	}
	out := make([]*Order, 0, len(b.orders))
	for _, o := range b.orders {
		if !removed[o.ID] {
			out = append(out, o)
		}
	}
	return out
}

// CancelledOrders 返回当日过期撤销的挂单(审计用)。
func (b *Broker) CancelledOrders() []*Order { return b.expired }

// OpenOrders 返回未完成委托。
func (b *Broker) OpenOrders() []*Order {
	out := make([]*Order, 0, len(b.pending))
	for _, o := range b.pending {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Trades 返回全部成交。
func (b *Broker) Trades() []*Trade { return b.trades }

// Cancel 撤销挂单;返回是否成功。
func (b *Broker) Cancel(orderID string) bool {
	for i, o := range b.pending {
		if o.ID == orderID {
			o.Status = "9"
			if o.ReleasedCash > 0 {
				b.portfolio.Cash += o.ReleasedCash
				o.ReleasedCash = 0
			}
			if o.FrozenAmount > 0 {
				pos := b.ensurePosition(o.Security)
				pos.EnableAmount = min64(pos.Amount, pos.EnableAmount+o.FrozenAmount)
				o.FrozenAmount = 0
			}
			b.expired = append(b.expired, o)
			delete(b.orderIndex, o.ID)
			b.pending = append(b.pending[:i], b.pending[i+1:]...)
			return true
		}
	}
	return false
}

// GetOrder 按 id 查委托。
func (b *Broker) GetOrder(id string) *Order { return b.orderIndex[id] }

// GetPosition 查持仓(只读;不存在时返回空持仓,不写入账户)。
func (b *Broker) GetPosition(sec string) *Position {
	if pos, ok := b.portfolio.Positions[sec]; ok {
		return pos
	}
	return &Position{Security: sec}
}

// ensurePosition 取持仓,不存在则创建并登记到账户。
func (b *Broker) ensurePosition(sec string) *Position {
	if pos, ok := b.portfolio.Positions[sec]; ok {
		return pos
	}
	pos := &Position{Security: sec}
	b.portfolio.Positions[sec] = pos
	return pos
}

func (b *Broker) register(order *Order) {
	b.orders = append(b.orders, order)
	b.orderIndex[order.ID] = order
}

// applyFill 记账一笔成交。
//
// cashValue/cashFeeValue 为负数时按成交额自动计算;量化差异来自部分成交时
// 按原始下单量计提费用与滑点(与 quantbt 的 _apply_fill 对齐)。
func (b *Broker) applyFill(sec string, amount int64, price float64, cashValue, cashFeeValue float64) {
	if amount == 0 {
		return
	}
	pos := b.ensurePosition(sec)
	value := float64(abs64(amount)) * price
	fees := b.fees(value, amount < 0)
	if amount > 0 {
		cv := value
		if cashValue >= 0 {
			cv = cashValue
		}
		feeValue := value
		if cashFeeValue >= 0 {
			feeValue = cashFeeValue
		}
		cashFees := b.fees(feeValue, false)
		oldValue := pos.CostBasis * float64(pos.Amount)
		pos.Amount += amount
		pos.TodayAmount += amount
		if pos.Amount > 0 {
			pos.CostBasis = (oldValue + value + fees) / float64(pos.Amount)
		}
		b.portfolio.Cash -= cv + cashFees
	} else {
		sellAmount := -amount
		remainingAmount := pos.Amount - sellAmount
		remainingCost := pos.CostBasis*float64(pos.Amount) - (value - fees)
		pos.Amount -= sellAmount
		pos.EnableAmount = max64(0, min64(pos.EnableAmount-sellAmount, pos.Amount))
		b.portfolio.Cash += value - fees
		if remainingAmount == 0 {
			pos.CostBasis = 0
		} else {
			pos.CostBasis = remainingCost / float64(remainingAmount)
		}
	}
	pos.LastSalePrice = price
	b.tradeSeq++
	b.trades = append(b.trades, &Trade{
		TradeID:   fmt.Sprintf("T%06d", b.tradeSeq),
		Security:  sec,
		Side:      map[bool]string{true: "buy", false: "sell"}[amount > 0],
		Amount:    abs64(amount),
		Price:     price,
		Value:     float64(abs64(amount)) * price,
		TradeTime: dayString(b.currentDay) + " 15:00:00",
	})
}

func (b *Broker) nextOrderID() string {
	return fmt.Sprintf("O%06d", len(b.orders)+len(b.expired)+1)
}

func (b *Broker) marketable(amount int64, limit, price float64) bool {
	if limit <= 0 {
		return true
	}
	if amount > 0 {
		return limit >= price
	}
	return limit <= price
}

func (b *Broker) executionPrice(price float64, amount int64) float64 {
	if b.slippage.Value == 0 {
		return price
	}
	dir := 1.0
	if amount < 0 {
		dir = -1
	}
	if b.slippage.Fixed {
		return price + dir*b.slippage.Value/2
	}
	return price * (1 + dir*b.slippage.Value/2)
}

func (b *Broker) fees(value float64, isSell bool) float64 {
	commission := value * b.commission.Cost
	if commission < b.commission.MinTradeCost {
		commission = b.commission.MinTradeCost
	}
	handling := value * HandlingFeeRate
	tax := 0.0
	if isSell {
		tax = value * b.commission.Tax
	}
	return commission + handling + tax
}

func (b *Broker) capVolume(sec string, amount int64, volume float64) int64 {
	if b.limitMode == "UNLIMITED" || amount == 0 || volume <= 0 {
		return amount
	}
	lot := int64(lotSize(sec))
	maxAmount := int64(volume*b.volumeRatio) / lot * lot
	used := b.volumeUsed[sec]
	remaining := float64(maxAmount) - used
	if remaining < 0 {
		remaining = 0
	}
	fill := int64(abs64(amount))
	if float64(fill) > remaining {
		fill = int64(remaining)
	}
	fill = fill / lot * lot
	b.volumeUsed[sec] = used + float64(fill)
	if amount > 0 {
		return fill
	}
	if fill > abs64(amount) {
		fill = abs64(amount)
	}
	return -fill
}

func (b *Broker) roundAmount(sec string, amount int64) int64 {
	lot := int64(lotSize(sec))
	if amount > 0 {
		return amount / lot * lot
	}
	if amount < 0 {
		pos := b.GetPosition(sec)
		if abs64(amount) >= pos.Amount {
			return -pos.Amount
		}
		return -(abs64(amount) / lot * lot)
	}
	return 0
}

func lotSize(sec string) int {
	if len(sec) >= 2 {
		switch sec[:2] {
		case "11", "12", "13":
			return 10
		}
	}
	return 100
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func dayString(days int64) string {
	return DaysToTime(days).Format("2006-01-02")
}
