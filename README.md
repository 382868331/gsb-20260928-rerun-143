# 游戏存档服务核心

虚构世界的存档/读档核心，无画面、无游戏逻辑。三个关注点完全分开：

- 对象身份（savecore/graph.go）：指针即身份。遍历对象图，每个已注册
  *struct 序列化为带 \$id 的节点，重复指针（共享、循环引用）输出
  {"\$ref": id}；恢复时先登记零值实例再填充字段，因此循环引用也能还原，
  共享对象不会被复制成多份。
- 版本迁移（savecore/registry.go）：每个类型在注册表中声明当前版本，
  迁移函数按“v1 到 v2 逐步升级”的链式方式作用于原始数据 map。旧存档可读；
  未来版本、缺迁移步骤、未知类型都返回明确的类型化错误：
  UnknownTypeError / MigrationError。
- 写入过程（savecore/store.go）：文件格式为
  magic | 长度 | CRC32 | JSON payload。保存时写临时文件、fsync、再
  rename 原子发布；中断只留下临时垃圾，上一个完整版本原地不动。读取时
  校验魔数、长度、校验和，半文件/位翻转一律报 CorruptionError。
- 大型对象图可使用 NewPartitionedStore：编码器按注册类型名选择分区，每个
  对象有全存档唯一 `$id`。`manifest.json` 记录根引用、节点类型/版本、所属
  分区以及每个分区 JSON 的 CRC32；分区文件仍使用独立 SVCR 校验帧。
- 按需加载时 OpenLoader 先核对清单并为所有节点建立零值占位实例。任何
  LoadPartition 都复用同一张身份表，因此跨分区共享对象和多层循环不会因
  加载顺序被复制；未加载分区的字段保持零值，LoadAll 必须全部成功才返回。
- 某个分区物理损坏时返回 PartitionCorruptionError，明确列出分区名、受影响
  对象 ID 与类型；不会把缺失分区包装成成功的半套图。
- Upgrade 先完整打开并迁移所有分区（含跨分区引用），确认后写入新一代目录，
  最后原子替换 current.json。发布前失败或中断时，旧 current 指针和旧快照
  仍可完整读取。

不执行任意代码：节点里的类型名只用于查显式注册表；实例只能由注册时
提供的 New 构造；禁止 interface 字段，杜绝反序列化 gadget。

测试（world/world_test.go，虚构世界：Player/Guild/Item）：

- 共享物品与 Player 到 Guild 循环引用恢复后的指针同一性；
- 未知类型 world.Dragon 明确失败；
- 手写 v1 存档（旧字段名 Nickname/XP/Title）跨版本恢复且循环不断；
- 未来版本、缺失迁移链失败；
- 保存中断（残留临时文件）后旧版本仍完整可读；
- 截断、翻字节、坏魔数等半文件损坏均被校验拦截。
- 分区测试覆盖共享物品、Player/Guild/Item 多层循环、任意分区加载顺序、
  未加载分区的旧版本资料、损坏分区范围报告，以及保存/迁移发布前中断。

运行：go test ./...
