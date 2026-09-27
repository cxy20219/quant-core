# -*- coding: utf-8 -*-
"""对拍探针 P07:多标的与组合账户(hands-on 组合、现值与持仓明细)。

- day2: 同时买入两只股票
- day3: 全部卖出其中一只
- day4: 按现值目标调仓
"""


def initialize(context):
    set_universe(["600570.SS", "600000.SS"])
    set_commission(commission_ratio=0.0003, min_commission=5.0)
    set_slippage(0.002)
    g.day = 0


def handle_data(context, data):
    g.day += 1
    if g.day == 2:
        order_value("600570.SS", 200000)
        order_value("600000.SS", 150000)
        log.info("day2 cash=%.2f value=%.2f", context.portfolio.cash, context.portfolio.portfolio_value)
        for code in ("600570.SS", "600000.SS"):
            pos = get_position(code)
            log.info("  pos %s amount=%s enable=%s", code, pos.amount, pos.enable_amount)
    elif g.day == 3:
        order_target("600000.SS", 0)
        log.info("day3 after sell cash=%.2f", context.portfolio.cash)
    elif g.day == 4:
        order_target_value("600570.SS", 100000)
        pos = get_position("600570.SS")
        log.info("day4 retarget amount=%s cash=%.2f", pos.amount, context.portfolio.cash)
