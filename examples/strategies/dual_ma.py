# -*- coding: utf-8 -*-
"""最小示例策略:双均线(日线),用于回测服务冒烟与文档。

参数(通过 --params '{"fast":5,"slow":20,"symbols":["600000.SH"]}' 传入):
  fast / slow: 均线窗口
  symbols:     股票池
  position_pct:单标的仓位比例
"""


def initialize(context):
    g.fast = int(params.get("fast", 5))
    g.slow = int(params.get("slow", 20))
    g.symbols = list(params.get("symbols", ["600000.SH"]))
    g.position_pct = float(params.get("position_pct", 0.95))
    set_universe(g.symbols)
    set_commission(commission_ratio=0.0003, min_commission=5.0)
    set_slippage(0.002)
    log.info("双均线策略启动: fast=%s slow=%s symbols=%s", g.fast, g.slow, g.symbols)


def handle_data(context, data):
    for code in g.symbols:
        if code not in data:
            continue
        hist = get_history(g.slow + 1, "1d", "close", code, fq="post", include=True)
        if len(hist) < g.slow + 1:
            continue
        closes = hist["close"]
        fast_now = closes[-g.fast:].mean()
        slow_now = closes[-g.slow:].mean()
        pos = get_position(code)
        holding = pos.amount > 0
        if fast_now > slow_now and not holding:
            order_target_value(code, context.portfolio.portfolio_value * g.position_pct)
            log.info("金叉买入 %s fast=%.3f slow=%.3f", code, fast_now, slow_now)
        elif fast_now < slow_now and holding:
            order_target(code, 0)
            log.info("死叉卖出 %s fast=%.3f slow=%.3f", code, fast_now, slow_now)
