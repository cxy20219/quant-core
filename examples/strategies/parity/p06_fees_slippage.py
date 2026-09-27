# -*- coding: utf-8 -*-
"""对拍探针 P06:费用与滑点变体(固定滑点/免滑点/自定义佣金)。

- day2: 固定滑点下买入
- day3: 切换为比例滑点后卖出
- day5: 自定义佣金(万二/最低 1 元)买入
"""


def initialize(context):
    set_universe(["600570.SS"])
    set_fixed_slippage(0.05)
    set_commission(commission_ratio=0.0003, min_commission=5.0)
    g.day = 0


def handle_data(context, data):
    g.day += 1
    if g.day == 2:
        order("600570.SS", 1000)
        log.info("day2 fixed-slippage cash=%.2f", context.portfolio.cash)
    elif g.day == 3:
        set_slippage(0.002)
        order("600570.SS", -1000)
        log.info("day3 ratio-slippage cash=%.2f", context.portfolio.cash)
    elif g.day == 5:
        set_commission(commission_ratio=0.0002, min_commission=1.0)
        order("600570.SS", 500)
        log.info("day5 custom-commission cash=%.2f", context.portfolio.cash)
