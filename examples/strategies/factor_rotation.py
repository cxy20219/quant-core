# -*- coding: utf-8 -*-
"""真实场景策略:因子选股(因子 + 市值过滤,月度调仓)。

验证"因子 → 策略"闭环:策略直接消费面板/接口注册的因子。
PTrade 要点:
  - 只能交易股票池内标的 → 调仓日把"目标 + 待卖出持仓"一起放进 set_universe;
  - 只在调仓日下单(用 g.rebalance_today 标记),避免每日重复交易。

依赖已注册因子(见 harness/workflow/factor-tracking.md),默认 reversal_5。
参数:factor_id / top_n / min_mv / max_mv
"""


def initialize(context):
    g.factor_id = params.get("factor_id", "reversal_5")
    g.top_n = int(params.get("top_n", 30))
    g.min_mv = float(params.get("min_mv", 50000))
    g.max_mv = float(params.get("max_mv", 500000))
    g.month = None
    g.targets = []
    g.rebalance_today = False
    set_commission(commission_ratio=0.0003, min_commission=5.0)
    set_slippage(slippage=0.002)
    log.info("FACTORROT|init|factor=%s|top_n=%d|mv=[%.0f,%.0f]" % (g.factor_id, g.top_n, g.min_mv, g.max_mv))


def before_trading_start(context, data):
    current_dt = context.blotter.current_dt
    month_key = (current_dt.year, current_dt.month)
    if g.month == month_key or current_dt.day < 25:
        return
    g.month = month_key

    scores = get_factor(g.factor_id)
    if not scores:
        log.warning("FACTORROT|no_factor|factor=%s" % g.factor_id)
        return
    frame = get_fundamentals(list(scores.keys()), "valuation", ["total_value"], None)
    if frame is None or len(frame) == 0:
        log.warning("FACTORROT|no_fundamentals")
        return

    picked = []
    for code, row in frame.iterrows():
        mv = row["total_value"] / 10000.0
        if g.min_mv <= mv <= g.max_mv and code in scores:
            picked.append((scores[code], code))
    picked.sort(reverse=True)
    g.targets = [code for _score, code in picked[: g.top_n]]

    holdings = [code for code, pos in context.portfolio.positions.items() if pos.amount > 0]
    set_universe(list(set(g.targets) | set(holdings)))
    g.rebalance_today = True
    log.info(
        "FACTORROT|plan|dt=%s|factor_values=%d|candidates=%d|targets=%d|holdings=%d"
        % (current_dt.strftime("%Y-%m-%d"), len(scores), len(picked), len(g.targets), len(holdings))
    )


def handle_data(context, data):
    if not g.rebalance_today or not g.targets:
        return
    g.rebalance_today = False

    # 先卖出不在目标池内的持仓(已在 set_universe 内,可正常成交)
    for code, pos in list(context.portfolio.positions.items()):
        if pos.amount > 0 and code not in g.targets:
            order_target(code, 0)

    # 目标池等权买入(含已持有部分,按目标金额补足)
    total_value = context.portfolio.portfolio_value
    per_target = total_value / len(g.targets)
    for code in g.targets:
        if code not in data:
            continue
        price = data[code].close
        if price <= 0:
            continue
        target_amount = int(per_target / price / 100) * 100
        current = context.portfolio.positions[code].amount if code in context.portfolio.positions else 0
        delta = target_amount - current
        if abs(delta) >= 100:
            order(code, delta)
