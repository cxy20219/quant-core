# 数据源插件与 Tushare 导入流程

## 适用场景

从外部数据源(tushare 官方或中转站)导入数据集到数据湖,或**频繁换源**时的装载/卸载/校验。

## 核心机制(sources.yaml)

数据源是**配置驱动的插件**,不需要重编译:

```yaml
sources:
  relay-b:
    kind: tushare-http          # 插件类型:内置 tushare-http / exec
    base_url: https://pcd.mobcvb.cn/tushare/pro/
    api_naming: underscore      # 接口命名:underscore(A 站用 hyphen)
    auth: {style: header, header: X-API-Key, key: "${QUANT_RELAY_B_KEY}"}
    limit: {default: 10000, max: 10000, min: 600}
    tls: {insecure_skip_verify: true}   # B 站自签证书
    probe: {detect: true}               # 识别"5 行样例"伪数据
    retry: {max: 8, backoff_ms: 1200}
  any-source:
    disabled: true              # 卸载:保留配置但停用
bindings:
  stock_basic: [relay-b, relay-a]   # 数据集 → 源优先级(按调用降级)
```

- **装载**:新增一段配置;或写一个 `kind: exec` 的外部命令插件(任何语言,JSON over stdin/stdout)。
- **卸载**:`disabled: true` 或删除配置段。
- **换源**:调整 `bindings` 顺序;密钥用 `${ENV}` 引用,实际值放项目根 `.env`(已 gitignore)。

## 前置条件

- `.env` 中有对应密钥(当前:A 站 `QUANT_RELAY_A_KEY`、B 站 `QUANT_RELAY_B_KEY`)。
- 部署环境下导入命令在 NAS 上执行:NAS 能直连两站,开发机可能被本机代理劫持(见 references/nas-server.md)。

## 关键步骤

1. **查看源与绑定**:

   ```bash
   quantd source list
   ```

2. **探活**(默认用 trade_cal 打一发):

   ```bash
   quantd source check
   ```

3. **单接口调试**(参数说明书式排查):

   ```bash
   quantd source call --name relay-b --api stk_limit --param trade_date=20240102
   ```

4. **换源前的一致性对比**(同参数打链上两源,比较行数/字段/首末行):

   ```bash
   quantd source compare --dataset trade_cal --param exchange=SSE --param start_date=20240101 --param end_date=20241231
   ```

5. **导入**(默认按 bindings 降级链,多源时按调用降级;重跑自动跳过已完成窗口):

   ```bash
   quantd import --dataset stock_basic --lake /vol1/quant-core/data/lake \
     --registry /vol1/quant-core/etc/datasets.yaml --sources /vol1/quant-core/etc/sources.yaml
   quantd import --dataset suspend_d --start 20240101 --end 20260927 ...   # 区间类
   quantd import --dataset stock_basic --source relay-a --replace ...      # 指定源 + 快照覆盖
   ```

6. **对账**(manifest 与实际行数):

   ```bash
   quantd verify --lake <lake> --dataset <dataset>
   ```

7. **去重与修账**(导入中断重跑产生的重复;断点续传已能避免新重复):

   ```bash
   quantd dedupe --dataset namechange --lake <lake>            # dry-run 看重复
   quantd dedupe --dataset namechange --lake <lake> --apply    # 执行去重(自动记账)
   quantd dedupe --dataset x --lake <lake> --reconcile         # 仅修账(实际与 manifest 不符时)
   ```

## 验证

- 成功信号:`source check` 输出 `[ok]`;`import` 输出 `dataset <x> imported: N rows`;`verify` 输出 `OK ... rows=N`。
- 数据语义校验(换源后必须做;已实测 B 站会忽略过滤条件返回全表/样例/子集):
  - 主键去重已在导入时自动执行(见 importSnapshot),dedupe 可复核;
  - 日期字段非空率(stock_basic 的 list_date 曾因源返回数字而全部为空,已修);
  - 与另一源的全表计数交叉核对(source compare);
  - `trade_cal` 覆盖率 = 区间内每个自然日一行(2024 SSE 应为 366 行,只含交易日=242 行属语义差异);
  - `stk_limit` 单日行数 ≈ 当日上市股票数(约 5500);
  - 单位:`suspend_d`/`index_daily` 字段与 tushare 一致。
- 常见失败信号:
  - `结果被截断(返回 x / 共 y)` / `返回行数恰好等于单次上限` → 缩小窗口或提高 limit(中转站 limit 语义不一致,见 experience/multi-source-ingestion)。
  - `upstream_pool_exhausted` → B 站上游池瞬时故障,窗口级重试 + 稍后重跑;持续失败可换源或等待。
  - `unknown api_name` → 该源没有此接口(如 A 站无 namechange),需换源或调整 bindings。
  - 导入中断后重跑 → 断点续传跳过已完成窗口;若在续传实现前产生重复,用 `dedupe`。

## 内置数据集规格(源无关)

| 数据集 | 模式 | 切片/窗口 | 说明 |
|---|---|---|---|
| stock_basic | snapshot | list_status × exchange(9 片) | A 站 5000 硬上限内 |
| trade_cal | snapshot | exchange × 年份(111 片) | 中转站窗口 ≤ 366 天 |
| index_basic | snapshot | market × 5 片 | |
| suspend_d | month-range | 按月 | |
| namechange | month-range | 按月 | 仅 B 站有 |
| index_daily | month-range | 10 个宽基指数 × 按年窗口 | 指数全量太大 |
| stk_limit | date-range | 逐日 | 单日全市场 ~5500 行 |
| bars_daily/bars_1m/adj_factor/corporate_actions | 迁移/离线 | — | 来自旧湖迁移 |

## 已知坑与修复(2026-09-27)

- **日期区间导入此前未去重**:`writeRows`(逐日区间路径)不像快照路径那样按主键去重,
  而中转站会对同一查询返回重复行(relay-b 的 stk_limit 单日常见 ~1,500 行重复),
  造成湖内重复数据。已修复:区间路径同样按主键去重并打印 `主键去重移除 N 行`。
- **CLI 导入此前未启用 Resume**:`cmdImport` 未设置 `Resume: true`,重跑会重复导入
  已完成窗口(配合上一条会产生成倍重复)。已修复。
- **`stk_limit` 不能绑 relay-a 作降级**:A 站对单日全市场返回恰好 5000 行(硬上限截断)
  且不含基金/ETF(同一天 B=6969 行、A=5000 行);导入器的截断检测会拒绝该窗口。
  因此 `sources.yaml` 中 `stk_limit` 只绑 `relay-b`。
- **上游覆盖随池漂移(重要)**:relay-b 的 stk_limit 单日行数在不同时间不同——
  有时 6,969 行(股票+基金/ETF),有时 5,469 行(仅股票);同一天重拉两次可能不同。
  这意味着:① 补数时不要盲目"全量重导"(会丢掉已有的基金覆盖),
  应先备份、只补缺失区间;② 湖内 stk_limit 的覆盖度按日不齐属源侧限制,
  对外说明时以"当日实际返回"为准;③ 跨源核对时注意 A 站还额外截断到 5000 行。
- 排查手法:`source compare --dataset <x> --param ...` 比两源行数;
  拉一天数据到临时湖(`import --source <s> --lake <tmp>`)后用 duckdb 比对代码集合,
  可直接看出缺的是哪类代码(如基金 159xxx/5xxxxx)。

## 最近验证

2026-09-27:导入 trade_cal(27,283)、stock_basic(5,871)、index_basic(10,777)、suspend_d(9,459)、namechange(1,212)、index_daily(25,301)全部通过 `verify`;
stk_limit:清理重复(497,846 → 281,220)后从 20250308 续传到 20260924,
新增 2,179,802 行,现共 2,461,022 行(488 窗口/376 文件,覆盖 2025-01-02 ~ 2026-09-24);
去重、Resume 与源绑定修复见上文"已知坑与修复";
注意:近年上游多为仅股票口径(5,644~5,648 行/日),早期含基金/ETF 的日子约 6,969 行,覆盖度按日不齐属源侧限制。
