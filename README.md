# 带世代句柄的伙伴内存分配模拟器

单线程 byte arena 伙伴分配模拟库。调用方通过带槽号和世代号的句柄访问数据；块被释放或调整大小后，旧句柄永久失效，即使地址和槽位被复用也不会复活。仅使用 Go 标准库，无 `unsafe`，Windows 原生离线可运行。

## 接口（`buddy` 包）

- `New(p int) (*Arena, error)` — 创建容量为 2^p 字节的 arena，`4 <= p <= 24`，最小块 16 字节。
- `Alloc(size, alignment int) (Handle, error)` — `size > 0`，`alignment` 为不超过容量的 2 次幂（相对 arena 偏移 0 对齐）。拆分最小可容纳块，同阶取最低偏移；新块清零；失败时不改变任何可见状态。
- `Free(h Handle) error` — 清零块并递归合并伙伴；世代号递增，重复释放/陈旧句柄报错；世代耗尽（uint64 上限）报 `ErrGenerationExhausted`，不回绕。
- `Read(h, buf) (int, error)` / `Write(h, data) error` — 仅在请求长度范围内拷贝，越界失败且不修改数据；不暴露底层切片。
- `Resize(h, newSize) (Handle, error)` — 保留原 alignment。缩小可释放伙伴；增长仅在保持原偏移时原地合并，否则申请新块迁移。成功保留前 `min(old,new)` 字节、清零增长部分、旧句柄失效；无新块时失败并完整保留旧句柄/数据/空闲结构。
- `Size(h)` / `Offset(h)` — 查询请求大小与 arena 偏移。
- `Audit() error` — 校验所有已用/空闲块不交叠、完整覆盖 arena、同阶无可合并伙伴遗留。

## 运行

```sh
go run ./cmd/demo                          # 演示：正常流程 + 迁移 + 陈旧句柄失败 + 碎片失败
go test ./... -count=1 -timeout=60s        # 全部测试
```

测试覆盖：对齐、拆分合并、同阶最低偏移、碎片失败及失败原子性、重复释放与地址复用不复活、Read 不泄漏可写别名、Resize 保留对齐、原地增长保持偏移、增长失败回滚、缩小释放伙伴、清零、世代耗尽，以及固定种子（20260919）随机操作序列对照逐字节参考模型的验证。
