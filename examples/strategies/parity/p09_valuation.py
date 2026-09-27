# -*- coding: utf-8 -*-
"""对拍探针 P09:估值表 get_fundamentals(valuation)。

- 单标的固定日期:float_value/pe_ttm/pb/turnover_rate/dividend_ratio(含百分比字段格式)
- 多标的:total_value/pe_dynamic/a_shares/a_floats(万元→元、万股→股换算)
- 空结果:非交易日查询返回空表(列结构仍完整)
"""


def initialize(context):
    set_universe(["600570.SS", "000001.SZ"])
    g.done = False


def handle_data(context, data):
    if g.done:
        return
    g.done = True

    frame = get_fundamentals(
        "600570.SS",
        "valuation",
        ["float_value", "pe_ttm", "pb", "turnover_rate", "dividend_ratio"],
        "2019-12-31",
    )
    log.info("P09|single|index=%s|columns=%s", list(frame.index), list(frame.columns))
    row = frame.iloc[0]
    log.info(
        "P09|single|trading_day=%s|float_value=%r|pe_ttm=%r|pb=%r|turnover_rate=%r|dividend_ratio=%r",
        row["trading_day"],
        row["float_value"],
        row["pe_ttm"],
        row["pb"],
        row["turnover_rate"],
        row["dividend_ratio"],
    )

    multi = get_fundamentals(
        ["600570.SS", "000001.SZ"],
        "valuation",
        ["total_value", "pe_dynamic", "a_shares", "a_floats"],
        "2019-12-31",
    )
    log.info("P09|multi|index=%s|columns=%s", list(multi.index), list(multi.columns))
    for code in ["600570.SS", "000001.SZ"]:
        log.info(
            "P09|multi|code=%s|total_value=%r|pe_dynamic=%r|a_shares=%r|a_floats=%r",
            code,
            multi.loc[code, "total_value"],
            multi.loc[code, "pe_dynamic"],
            multi.loc[code, "a_shares"],
            multi.loc[code, "a_floats"],
        )

    empty = get_fundamentals("600570.SS", "valuation", ["pb"], "2019-12-28")
    log.info("P09|empty|shape=%s|columns=%s", tuple(empty.shape), list(empty.columns))
