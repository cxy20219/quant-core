"""quant-core 数据服务使用示例。

把 tushare SDK 的 HTTP 地址指向 quantd 即可,接口、字段与单位与 tushare 一致。

运行:
    python examples/use_data_service.py [http://<nas-host>:8000]
"""

from __future__ import annotations

import sys
import warnings

warnings.filterwarnings("ignore")

import tushare as ts


def make_pro(url: str):
    pro = ts.pro_api("local")
    # tushare SDK 通过这个私有属性指定 HTTP 入口;quantd 实现了同一协议。
    pro._DataApi__http_url = url
    return pro


def main() -> int:
    url = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8000"
    pro = make_pro(url)

    print("== 日线行情 ==")
    df = pro.daily(ts_code="600000.SH", start_date="20240102", end_date="20240110")
    print(df.to_string(index=False))

    print("\n== 每日指标(估值) ==")
    df = pro.daily_basic(ts_code="600000.SH", trade_date="20240102",
                         fields="ts_code,trade_date,close,turnover_rate,pe,pb,total_mv")
    print(df.to_string(index=False))

    print("\n== 复权因子 ==")
    df = pro.adj_factor(ts_code="600000.SH", start_date="20240102", end_date="20240110")
    print(df.to_string(index=False))

    print("\n== 分钟线(1min) ==")
    df = pro.stk_mins(ts_code="600000.SH", freq="1min",
                      start_date="20240102 09:30:00", end_date="20240102 10:00:00")
    print(df.head(10).to_string(index=False))

    print("\n== 股票列表 ==")
    df = pro.stock_basic(exchange="", list_status="L", fields="ts_code,name,industry,list_date")
    print(f"{len(df)} 只标的;示例:")
    print(df.head(5).to_string(index=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
