# -*- coding: utf-8 -*-
"""真实场景策略:小市值轮动(月度调仓,全市场取池)。

这是"真实可用性"验证,不是对拍:验证 get_Ashares / get_fundamentals / set_universe /
order_target 的组合能跑通一个贴近实盘的策略。PTrade 惯用法:盘前算池并 set_universe,
handle_data 里下单(盘前 set_universe 当日的 data 即可见)。

参数:top_n(持仓数,默认 30)、min_mv(最小市值,万元,默认 50000)、max_mv(默认 500000)
"""


def initialize(context):
    g.top_n = int(params.get("top_n", 30))
    g.min_mv = float(params.get("min_mv", 50000))
    g.max_mv = float(params.get("max_mv", 500000))
    g.rebalanced_month = None
    g.targets = []
    set_commission(commission_ratio=0.0003, min_commission=5.0)
    set_slippage(slippage=0.002)
    log.info("SMALLCAP|init|top_n=%d|mv=[%.0f,%.0f]" % (g.top_n, g.min_mv, g.max_mv))


def before_trading_start(context, data):
    current_dt = context.blotter.current_dt
    month_key = (current_dt.year, current_dt.month)
    if g.rebalanced_month == month_key or current_dt.day < 25:
        return
    g.rebalanced_month = month_key

    codes = get_Ashares()
    if not codes:
        log.warning("SMALLCAP|no_universe")
        return
    frame = get_fundamentals(codes, "valuation", ["total_value"], None)
    if frame is None or len(frame) == 0:
        log.warning("SMALLCAP|no_fundamentals")
        return

    picked = []
    for code, row in frame.iterrows():
        mv = row["total_value"] / 10000.0  # 元 -> 万元
        if g.min_mv <= mv <= g.max_mv:
            picked.append((mv, code))
    picked.sort()
    g.targets = [code for _mv, code in picked[: g.top_n]]
    set_universe(g.targets)  # 盘前设定当日可见范围
    log.info(
        "SMALLCAP|plan|dt=%s|candidates=%d|targets=%d"
        % (current_dt.strftime("%Y-%m-%d"), len(picked), len(g.targets))
    )


def handle_data(context, data):
    if not g.targets:
        return
    # 卖出不在目标池内的持仓
    for code, pos in list(context.portfolio.positions.items()):
        if pos.amount > 0 and code not in g.targets:
            order_target(code, 0)
    # 目标池等权买入
    cash_per = context.portfolio.cash / len(g.targets)
    for code in g.targets:
        if code not in data:
            continue
        price = data[code].close
        if price <= 0:
            continue
        amount = int(cash_per / price / 100) * 100
        if amount > 0:
            order(code, amount)
