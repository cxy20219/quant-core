# Tushare 协议兼容要点

## 现象

用官方 tushare SDK(`ts.pro_api()` + `pro._DataApi__http_url = <quantd>`)对接时,
出现"字段缺失 / 单位不一致 / 请求被拒 / 结果为空"。

## 协议要点

- HTTP 协议与 tushare pro 一致:`POST /`
  body `{"api_name","token","params","fields"}`,响应
  `{"code":0,"msg":null,"data":{"fields":[...],"items":[[...]],"has_more":false}}`;
  错误用非 0 `code` + `msg`。SDK 私有属性 `pro._DataApi__http_url` 指向本服务即可。
- `fields` 是**白名单**:接口声明 `SelectFields` 后,请求只能取子集;
  未在白名单的字段(如 `daily` 请求 `pe_ttm`)必须报错而不是静默返回。
- 分页:请求 `params.limit/offset`;服务端读 `limit+1` 行判断 `has_more`;
  未指定时用接口默认上限(daily 6000)。全市场单日约 5256 行,不会截断。
- 字段与单位(内部存储口径 vs 接口输出):
  - `daily`:vol=手、amount=千元、pct_chg=百分数、trade_date=YYYYMMDD 字符串 → 与内部一致,零转换。
  - `daily_basic`:total_share=万股、total_mv=万元 → 与内部一致。
  - `stk_mins`:**接口输出 vol=股、amount=元**(tushare 口径),内部是手/千元,
    由 `API.Transforms` 做 ×100 与 ×1000 换算。
- `adj_factor`:tushare 语义只返回**一个**复权因子(后复权 hfq)。
  本湖同时存 hfq/qfq 两个分区,接口默认 `FixedPartitions: {factor_type: hfq}`,
  传 `factor_type=qfq` 可取前复权;不做过滤会返回双份行(踩过的坑)。
- `stk_mins` 当前只支持 `freq=1min`;其他频率(5/15/30/60min)会明确报错,
  尚未实现由 1min 聚合(见 `internal/tsapi/apis.go` 的 `minsFilter`)。
- 快照类接口(stock_basic/trade_cal/index_basic)按 `exchange`/`market`/`list_status`
  过滤,这些维度在湖里是**普通字段**(非分区)。

## 做法

- 新增接口:在 `internal/tsapi/apis.go` 注册 `API{Dataset, BuildFilter, SelectFields, Transforms, FixedPartitions, DefaultLimit}`,
  过滤沿用 `codeDateFilter` 或自定义;新增数据集先加进 `schemas/datasets.yaml`。
- 兼容性验收:

  ```bash
  python harness/scripts/accept_deploy.py --url http://<nas-host>:8000 --full
  ```

  覆盖 daily/daily_basic/adj_factor/stk_mins 与分钟-日线 vol 交叉校验。
- 延迟测量:

  ```bash
  python harness/scripts/bench_api.py --url http://<nas-host>:8000
  ```

  验证:输出各查询的 p50/p95;2026-09-26 基线为 daily 单代码一年 p50≈49ms、
  stk_mins 单代码单日 p50≈51ms;若显著劣化(>2 倍),按
  `harness/experience/scan-engine-pruning.md` 排查剪枝与缓存。

- 请求/响应样例(含各接口单位说明)见 `harness/assets/tushare-api-examples.json`。
- 单位类问题用交叉校验定位:同一标的一天,`stk_mins.vol` 合计应等于
  `daily.vol × 100`(diff 0.0% 为通过)。

## 边界

- 尚不支持:tushare 的财务接口(income/balancesheet 等)、`pro_bar` 复权行情、
  `freq != 1min`;这些需要新增数据集或聚合实现。
- token 校验仅在 `QUANTD_TOKENS` 非空时启用;内网匿名可用。

## 关联

- `harness/scripts/accept_deploy.py`、`harness/scripts/bench_api.py`
- 实现:`internal/tsapi/`、`internal/source/tushare/`
