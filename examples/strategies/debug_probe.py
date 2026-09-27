# -*- coding: utf-8 -*-
"""调试策略:检查数据可见性与 get_history 返回形态。"""


def initialize(context):
    set_universe(["600000.SH"])
    log.info("initialize done")


def handle_data(context, data):
    if not getattr(g, "logged", False):
        g.logged = True
        log.info("data keys=%s", list(data.keys()))
        bar = data.get("600000.SH")
        log.info("bar type=%s close=%s", type(bar).__name__, getattr(bar, "close", None))
        hist = get_history(25, "1d", "close", ["600000.SH"], fq="post", include=True)
        log.info("history type=%s len=%s", type(hist).__name__, len(hist))
        try:
            log.info("history columns=%s", list(hist.columns))
        except Exception as exc:  # noqa: BLE001
            log.info("columns error: %s", exc)
        if isinstance(hist, dict):
            log.info("dict keys=%s", list(hist.keys()))
        else:
            try:
                series = hist["600000.SH"]["close"]
                log.info("series tail=%s", list(series.tail(3)))
            except Exception as exc:  # noqa: BLE001
                log.info("series error: %s", exc)
    # 无交易
