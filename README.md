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

不执行任意代码：节点里的类型名只用于查显式注册表；实例只能由注册时
提供的 New 构造；禁止 interface 字段，杜绝反序列化 gadget。

测试（world/world_test.go，虚构世界：Player/Guild/Item）：

- 共享物品与 Player 到 Guild 循环引用恢复后的指针同一性；
- 未知类型 world.Dragon 明确失败；
- 手写 v1 存档（旧字段名 Nickname/XP/Title）跨版本恢复且循环不断；
- 未来版本、缺失迁移链失败；
- 保存中断（残留临时文件）后旧版本仍完整可读；
- 截断、翻字节、坏魔数等半文件损坏均被校验拦截。

运行：go test ./...

