# 游戏存档服务核心（Go）

面向对象图的完整存档/恢复核心。只处理游戏状态，不含任何画面或通用文件管理功能。

## 设计要点

- **对象身份、迁移、写入过程三者分离**
  - 身份：`registry.go` 按 Go 指针类型注册（类型名 + 版本），`encode.go` 按
    **指针地址**去重。同一对象在图中出现任意次、包括循环引用，文件中只写一份。
  - 迁移：`Registry` 保留所有历史版本；旧版本通过 `RegisterLegacy` 登记，
    由当前版本类型实现 `Migrator`（纯数据映射，输入/输出都是字段表）升级。
  - 写入：`store.go` 写临时文件 → `fsync` → 原子 `rename` → 目录 `fsync`，
    中断时只留下/读到上一个完整版本，临时文件自动回滚清理。
- **文件格式**（`wire.go`）：单个 JSON 文档，对象表 + 命名根。引用编码为
  `{"kind":"ref","id":N}`，对象按 id 顺序排列，天然支持前向引用与环。
- **恢复是两遍解码**（`decode.go`）：先校验类型并实例化全部对象，再统一回填
  字段、最后一次性接线引用，因此共享对象与循环恢复后仍然是同一批对象，
  绝不会复制成多个。
- **未知类型明确失败**：类型名或版本未注册时返回 `*UnknownTypeError`，
  在校验阶段就中止，不猜测、不跳过、不新建对象。
- **读取不执行任意代码**：存档只含 JSON 数据。能被实例化的类型仅限于游戏
  显式注册的构造器；文件里没有任何类型加载/函数调用入口。接口字段只接受
  纯数据（数字、字符串、布尔），引用标记放进非指针字段会直接报错。
- **半文件损坏**：截断/非法 JSON/错误魔数/错误格式版本都归类为
  `*CorruptSaveError`，可用 `gamesave.IsCorrupt(err)` 区分。

## 目录

- `*.go`：存档核心包 `gamesave`（注册表、编解码、原子存储）。
- `gameworld/`：虚构世界（`Player` v1→v2、`Chest`），演示共享对象、环与迁移。
- `gameworld/testdata/save_player_v1.json`：签入的旧版本存档夹具。
- `gameworld/demo/`：端到端演示（往返 + 读取 v1 存档）。

## 运行

	go test ./...
	go run ./gameworld/demo

演示输出包括：玩家↔宝箱循环保持、共享朋友不被复制、v1 存档迁移为 v2
（`gold=250` → `coins=250, banked=0, hometown=Brindlebrook`）。

## 版本迁移示例

在新版本类型上实现 `gamesave.Migrator` 并登记旧版本：

	reg.MustRegister("player", 2, func() gamesave.Object { return &Player{} })
	reg.MustRegisterLegacy("player", 1)

`(*Player).Migrate(1, oldFields)` 返回 v2 字段表；引用标记原样透传，
由核心在迁移后统一重新接线。

## 测试覆盖

- 共享对象 / 直接环 / 自引用 / 跨根共享：`TestSharedAndCycleIdentity`
- 未知类型、未知版本：`TestUnknownTypeFails`、`TestUnknownVersionFails`
- 写入中断保留旧版本（含临时文件清理）：`TestSaveFailureKeepsPreviousVersion`
- 半文件损坏：`TestTruncatedFileIsCorruptAndUnchanged`
- 引用不可用于非指针字段（拒绝夹带“类型描述”）：`TestNoCodeExecutionFromSave`
- 悬空引用：`TestDanglingAndDuplicateRefsRejected`
- 跨版本恢复与再保存：`gameworld` 包两个测试
