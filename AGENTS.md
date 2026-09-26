> 在执行任何操作前，必须先根据下方文件索引查阅获取足够上下文，确认信息充分后再行动；`harness/harness.md` 是必须查阅的项目总览。
# 文档索引，按需查阅

- [Harness 开发指南](harness/harness.md)：项目总览、关键约束、常用入口和注意事项
- [NAS 部署流程](harness/workflow/deploy-nas.md)：交叉编译、镜像构建、数据上传、容器验收与回滚
- [数据湖迁移流程](harness/workflow/data-lake-migration.md)：旧湖盘点、源文件归一化、迁移、逐分区对账
- [Tushare 数据导入流程](harness/workflow/tushare-data-import.md)：数据集导入模式选择、token 前置、增量与快照
- [Parquet 生态兼容坑](harness/experience/parquet-ecosystem-compatibility.md)：parquet-go 与 pyarrow/duckdb 的编码、ordinal 与统计差异
- [扫描引擎剪枝经验](harness/experience/scan-engine-pruning.md)：排序布局、行组大小、元数据缓存与剪枝失效排查
- [Tushare 协议兼容要点](harness/experience/tushare-protocol-compat.md)：字段/单位/分页/复权语义与接口白名单
- [NAS 服务器环境](harness/references/nas-server.md)：连接方式、Docker 配置、磁盘布局与网络性能
- [迁移对账脚本](harness/scripts/verify_migration.py)：旧湖与新湖逐分区行数核对
- [源文件归一化脚本](harness/scripts/normalize_source.py)：行组 ordinal 溢出的源文件排序重写
- [部署验收脚本](harness/scripts/accept_deploy.py)：健康检查、接口验收、单位交叉校验与延迟基准
- [接口基准脚本](harness/scripts/bench_api.py)：tushare 兼容接口的延迟分位测量
- [数据湖上传脚本](harness/scripts/upload_lake.py)：目录树上行 NAS,断点续传
- [远程命令脚本](harness/scripts/remote_exec.py)：在 NAS 上执行命令(支持 sudo 与脚本文件)
- [Tushare 接口样例](harness/assets/tushare-api-examples.json)：各接口请求/响应样例与单位说明
