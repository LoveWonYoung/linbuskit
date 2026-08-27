# AGENTS.md

本文件适用于整个仓库。修改代码前先阅读本文件；更深层目录若以后增加自己的 `AGENTS.md`，以更深层文件的规则为补充或覆盖。

## 项目定位

`linbuskit` 是 Go 编写的 LIN 总线诊断库，不是 CLI。它分为四层：

- `liniface/`：最小驱动接口和跨实现共享的数据类型、错误。
- `driver/`：硬件驱动。PCAN、Vector、TSMaster、Windows Toomoss 依赖 Windows DLL；Darwin Toomoss 使用 cgo 动态加载库。
- `tplin/`：LIN 诊断传输协议、主从站封装和模拟网络。
- `uds_client/`：面向单个目标 NAD 的 UDS over LIN 请求/响应客户端。

修改应优先保证协议正确性、并发安全、硬件边界安全和公共 API 稳定性。不要为了局部简洁破坏不同驱动的一致行为。

## 开始工作前

- 先运行 `git status --short`，保留用户已有修改，不清理或覆盖无关文件。
- 阅读目标包及其相邻测试；涉及 `liniface.Driver` 时同时检查至少一个真实驱动和 `SimulatedLinDriver`。
- `go.mod` 声明 Go 1.24。新增代码应保持 Go 1.24 兼容，不以本机更高版本独有特性为前提。
- 未经明确要求，不改变模块路径、导出标识符、默认超时、队列容量或硬件默认配置。
- 不提交 DLL、dylib、测试二进制、日志、IDE 文件或设备私有配置。

## 核心协议不变量

以下规则属于行为契约，相关改动必须由测试覆盖：

- LIN 帧 ID 的有效范围是 `0x00..0x3F`，帧数据最多 8 字节。
- 诊断请求/响应帧分别使用 `0x3C` 和 `0x3D`，并使用 Classic Checksum。
- `LinEvent.Channel` 必须在写入、调度、回环和接收全过程保持正确，不能隐式退回通道 0。
- 驱动读超时的统一语义是 `(nil, nil)`；驱动已关闭和通道无效优先包装 `liniface.ErrDriverClosed`、`liniface.ErrInvalidChannel`，以便 `errors.Is` 判断。
- TP 的 12 位长度包含 SID，不包含 NAD 和 PCI。用户数据最大 4094 字节。
- 单帧可承载 `SID + data <= 6` 字节；首帧承载 SID 和前 4 字节数据；连续帧每帧最多 6 字节。
- 连续帧序号从 1 开始并按 4 位值回绕；乱序、短帧、非法长度和超时必须丢弃当前重组状态，不能交付半包。
- 多帧入队必须是原子的：容量不足时返回 `ErrTxQueueFull`，不能只入队一部分帧。
- 广播 NAD 可以接收任意实际响应 NAD；非广播请求必须过滤其它 NAD。负响应还必须匹配原请求 SID。

## 内存与并发约定

- `LinEvent.EventPayload`、`LinMessage.Data` 等切片跨 goroutine、队列、日志或驱动边界时必须明确所有权。默认做深拷贝；只有调用者和被调用者都同步且契约明确时才允许借用。
- 不要修改调用者传入的 `LinEvent`。补写 `Channel`、`Direction`、`Timestamp` 时，应先复制事件及其 payload。
- 返回日志、缓存或响应的“副本”时必须深拷贝内部切片，不能只复制结构体或指针切片。
- `Run`、`Close`、`Stop` 应可重复调用，并在并发调用下保持安全。关闭必须能唤醒阻塞接收者，且不能遗留 ticker、timer 或 goroutine。
- 不在持锁期间调用未知回调、可能阻塞的驱动调用或向有界 channel 阻塞发送。必须阻塞时，提供 context/关闭信号。
- 热路径避免反复 `time.After` 和固定间隔轮询。优先使用已有的阻塞接收、可复用 timer 或 context，并用 benchmark/测试证明行为未改变。
- 队列溢出不能静默发生。库代码优先返回或上报可判断的错误；日志只作为补充，不能替代错误通道。

## 驱动和 FFI 规则

- 原生 API 的函数签名、结构体大小、字段偏移和 32/64 位差异必须对照厂商头文件。不要凭相似驱动推断。
- 传入 DLL/cgo 的 Go 内存必须在调用期间保持存活；严格校验长度后再构造 `unsafe.Pointer` 或 slice。
- DLL 调用与 `Close` 之间使用统一锁和状态检查，避免卸载后继续调用函数地址。
- 构造失败要按初始化的逆序释放已获得资源。`Close` 的后续调用应返回同一个已记录结果，而不是丢失第一次关闭错误。
- 真实驱动必须一致校验 nil event、帧 ID、payload 长度、通道和主从模式。
- 平台相关实现尽量把纯 Go 的配置校验、编码解码和协议逻辑抽成可在无硬件环境测试的函数。
- 修改 Windows ABI 或 `unsafe` 代码时，保留并扩展结构布局测试；最终还需在 Windows 真实硬件上做烟雾测试。

## API、错误和日志

- 新增导出类型、函数、方法、字段和常量时写 GoDoc，并说明所有权、并发安全、超时和关闭语义。
- 可分类错误使用包级 sentinel 或具体错误类型，并通过 `%w` 保留错误链。不要为同一状态创建多种无法 `errors.Is` 的字符串错误。
- 库内错误文本使用简洁英文，包含操作上下文；不要把协议错误仅写到全局 `log` 后吞掉。
- 日志不得输出敏感诊断数据，且高频收发日志必须受显式开关控制。不要在默认路径持续打印。
- `driver/` 内禁止直接调用 `fmt.Print*` 或 `log.Print*`。所有设备日志必须经过 `logDriverf`、`logLINMessage`、`logLINHeader` 或 `logLINNoResponse`，确保只在 `SetPrintLog(true)` 后输出，并保持统一 key-value 格式。
- 公共 API 或支持平台变化时同步更新 `README.md` 示例与平台说明。
- 兼容性不明确时优先新增配置或方法；不要直接改变现有默认行为。明显的拼写型公共 API（例如已有方法名）也需保留兼容入口再迁移。

## 测试与验证

所有 Go 代码在提交前运行：

```bash
gofmt -w <changed-go-files>
go test ./...
go test -race ./...
go vet ./...
```

测试应满足：

- 协议解析使用表驱动测试，覆盖边界长度、空/短帧、最大长度、CF 回绕、乱序、超时、错误 NAD/SID 和队列满。
- 并发与生命周期修改必须增加重复 `Run/Close/Stop`、并发关闭、关闭时阻塞读写的测试。
- 多通道修改至少覆盖两个通道交错收发，防止响应串线。
- 新测试不要依赖不受控的 `time.Sleep`。优先用 context、channel、deadline 和最终一致断言；必须等待真实时钟时给 CI 留出余量。
- 单元测试默认不能要求连接真实硬件或安装厂商 DLL。

在非 Windows 主机修改 Windows 驱动时，至少做交叉编译与静态检查：

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c ./driver -o /tmp/linbuskit-driver.test.exe
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c ./tplin -o /tmp/linbuskit-tplin.test.exe
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c ./uds_client -o /tmp/linbuskit-uds-client.test.exe
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./...
```

当前基线中，Windows `go vet` 会在 `driver/tsmaster.go` 的回调指针和 `driver/vector.go` 的错误字符串指针处各报告一条 `possible misuse of unsafe.Pointer`。这是现有厂商 FFI 边界的已知告警，不要仅为消除告警改写可工作的指针调用；不得新增同类告警。交叉编译不能替代 Windows DLL/设备测试。

文档或纯注释改动可按风险缩减验证，但交付时要明确实际运行过哪些命令。

## 已确定的设计决策

- 不再提供 `driver.MockDriver`；无硬件联调统一使用 `tplin.SimulatedLinNetwork`，包内测试可定义最小专用 fake。
- `LinSlave.Run/Stop` 与 Transport 关闭必须幂等；阻塞接收必须响应关闭。
- 异步边界统一深拷贝事件 payload，不修改调用者传入的 `LinEvent`。
- UDS 默认不重试。只有同时配置 `MaxRetries` 和 `RetryableSIDs[requestSID]` 才允许对超时请求重试。
- NRC `0x78` 使用独立的 `ResponsePendingTimeout`（P2*）；外部 context 始终是请求总上限。
- UDS 接收使用 Transport 阻塞接收，不恢复固定 2ms 轮询。
- 小型固定帧直接分配，除非 benchmark 证明有收益，不为 8 字节 payload 使用 `sync.Pool`。
- 所有设备日志默认关闭，由 `SetPrintLog` 统一控制；设备名、方向、通道、ID、长度、校验和与数据字段保持一致。

## 后续优化优先级

1. **Windows 实机验证**：在 Windows 上验证 PCAN、Vector、TSMaster、Toomoss 的构建、DLL 加载、收发与关闭；保留已知的两条 FFI vet 告警记录。
2. **硬件驱动可维护性**：在可执行 Windows 验证时再拆分接近千行的驱动文件，避免当前仅做机械移动却无法验证 ABI。
3. **可观测性**：统一可注入日志接口和错误分类，替换默认高频全局日志。
4. **CI 矩阵**：建立 Linux/macOS/Windows 编译、测试、race 和 vet 流程；硬件测试继续作为独立手工烟雾测试。
5. **仓库卫生**：清理已跟踪的系统文件，并收敛 `.gitignore` 中与项目无关的本机规则。

## 完成标准

- 改动范围小且与任务直接相关，没有顺手重写无关代码。
- 新行为有测试，失败路径与边界条件有覆盖。
- 相关平台的构建/测试结果已记录；不能执行的硬件验证明确列为未验证项。
- 公共 API、README、注释和实际行为保持一致。
- `git diff` 中不包含用户原有修改、生成二进制或本机环境文件。
