# 带世代句柄的伙伴内存分配模拟器

单线程字节 arena 伙伴分配模拟库（纯 Go 标准库，无 unsafe）。调用方通过带槽号与世代号的
`Handle` 访问数据；释放或 Resize 成功后旧句柄永久失效，地址复用也不会复活。

## 接口（包 `buddy`）

- `New(p int) (*Arena, error)` — 创建容量 2^p 字节的 arena，`4 <= p <= 24`，最小块 16 B。
- `Alloc(size, alignment int) (Handle, error)` — `size > 0`，`alignment` 为不超过容量的 2 次幂，
  对齐相对 arena 偏移 0。拆分最小可容纳块，同阶取最低偏移；失败不改变可见状态。
- `Free(h Handle) error` — 清零并递归合并伙伴；重复释放/陈旧句柄返回 `ErrStaleHandle`。
- `Read(h, off, dst) / Write(h, off, data) error` — 仅拷贝，不暴露底层切片；
  越界返回 `ErrOutOfBounds` 且不修改任何数据。
- `Resize(h, newSize) (Handle, error)` — 保留原 alignment；缩小释放多余伙伴，增长优先原地
  合并（保持偏移），否则迁移。成功保留前 `min(old,new)` 字节、清零增长部分、旧句柄失效；
  失败时旧句柄/数据/空闲结构全部保留。
- `Audit() error` — 校验已用/空闲块不交叠、完整覆盖 arena、无可合并伙伴遗留。
- 辅助：`Offset/Size/Alignment(h)`、`Capacity()`、`FreeBytes()`。

世代号单调递增不回绕，耗尽时报 `ErrGenerationExhausted`。

## 运行

Windows 原生 Go 1.26.5，仅标准库，无第三方依赖。

```sh
go run ./cmd/demo                          # 约 3 秒：正常流程 + 迁移 + 陈旧句柄/碎片失败
go test ./... -count=1 -timeout=60s        # 单元测试 + 固定种子随机参考模型验证
```

测试覆盖：对齐、拆分合并、碎片失败、重复释放、Read 不泄漏可写别名、Resize 保留对齐、
增长失败回滚、清零，以及固定种子随机操作序列对照逐字节参考模型的验证（含失败原子性）。
