# -*- coding: utf-8 -*-
"""对拍探针 P01:市价买卖基础(现金账本/费用/持仓)。

用固定日期与固定标的,确保两个引擎看到完全相同的行情。
标的:600570.SS(恒生电子);区间:2020-01-02 ~ 2020-01-10(日线)。
"""


def initialize(context):
    set_universe(["600570.SS"])
    set_commission(commission_ratio=0.0003, min_commission=5.0)
    set_slippage(0.002)
    g.day = 0


def handle_data(context, data):
    g.day += 1
    if g.day == 2:
        # 市价买入 1000 股
        order("600570.SS", 1000)
        log.info("BUY 1000 day=%s cash=%.2f", g.day, context.portfolio.cash)
    elif g.day == 4:
        # 市价卖出 1000 股
        order("600570.SS", -1000)
        log.info("SELL 1000 day=%s cash=%.2f", g.day, context.portfolio.cash)
