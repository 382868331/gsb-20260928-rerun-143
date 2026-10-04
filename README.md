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



## 区域分支与合并（第二轮新增）

同一保存点可以分出两个候选，各自编辑不同区域，再合成正式存档：

- 稳定身份（savecore/branch.go）：Fork/CloneBranch 分出分支时，每个对象
  获得稳定 ID，清单（稳定 ID ↔ 存档内节点 ID、类型、所属区域）随存档
  一同保存（<存档>.manifest.json）。跨候选比较按稳定身份进行，因此
  “移动到别的区域”不会被误判成“删除+新增”。
- 区域级比较（savecore/merge.go）：三方（base/A/B）按对象对比，区分
  added/deleted/modified/moved，标记共享对象；冲突类型包括
  modify-modify、delete-modify、move-move、add-add。候选数据版本不同
  时按迁移后的对象含义比较；无法转换的节点保留原始版本与数据原样
  参与合并。
- 合并会话（savecore/session.go）：操作者先 AdoptRegion 采用无冲突
  区域，再 Resolve 逐个处理冲突；UndoRegion 只撤回对应区域及其引用
  后果。每次决定立即落盘（<输出>.merge.json），中断后
  ResumeMergeSession 继续；候选文件从不被改写，始终可恢复。
- 正式存档不谎报：Finalize 始终写出可加载的存档，未采用的内容回退
  到 base 版本，并在存档内嵌的 merge 报告中逐一列明
  （pendingObjects / pendingReferences，complete=false）。被删除却仍
  被保留对象引用的目标，从 base 救回并明示，跨区域引用不会产生
  重复对象或悬空目标。

测试（world/merge_test.go）覆盖：无冲突区域先行采用、共享对象与跨区
引用身份保持、跨区移动识别、删除-修改冲突与未采用清单、被删目标的
引用救回、不同数据版本候选的比较与原始资料保留、中断重开、撤销采用。
