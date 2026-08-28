# linbuskit

## Toomoss ELINS

Open one Toomoss instance and add ELINS slave channels to it. Ordinary LIN and
ELINS can stay initialized together, including on the same physical channel.

```go
dev, err := driver.NewToomoss([]driver.ToomossCh{driver.CH1}, driver.SlaveMode)
if err != nil {
	return err
}
defer dev.Close()

err = dev.ConfigureELINS(driver.ELINSConfig{
	Channels:  []driver.ToomossCh{driver.CH1, driver.CH2},
	Baudrate:  2_000_000,
	ResEnable: 1,
	Version:   driver.ELINS_VER_IND83220,
})
if err != nil {
	return err
}

messages, err := dev.ElinsSlaveRead(driver.CH1)
```

`linbuskit` 是一个面向 Go 的 LIN 总线诊断工具库，覆盖了从底层驱动抽象、LIN Transport Protocol（单帧/多帧收发），到面向诊断场景的主站/从站封装，以及一个可直接使用的 UDS over LIN 客户端。

当前仓库更适合作为库集成到上位机、产线工具、诊断脚本或测试程序中，而不是一个开箱即用的 CLI。

## 特性

- 抽象统一的 LIN 驱动接口，便于接入真实硬件或模拟环境
- 内置 LIN 诊断传输层，支持单帧、首帧、连续帧的收发与重组
- 提供主站 `tplin.LinMaster`，封装常见诊断命令
- 提供从站 `tplin.LinSlave`，可模拟 ECU 节点并处理基础诊断服务
- 提供 `uds_client.Client`，用于发送 UDS 请求并处理正响应、负响应和超时
- 提供 `preset.Preset`，一站式创建硬件驱动与指定 NAD/通道的 UDS 客户端
- 提供模拟网络 `tplin.SimulatedLinNetwork`，方便联调和单元测试
- Windows 下支持 Toomoss、PCAN/PLIN、Vector XL LIN 和 TSMaster，macOS（Darwin + cgo）下支持 Toomoss

## 包结构

```text
liniface/     底层接口定义，包含 Driver、LinEvent、校验类型等
tplin/        LIN 诊断传输层、主站/从站封装、模拟网络
uds_client/   更高层的 UDS over LIN 客户端
driver/       硬件驱动实现（Windows 多厂商、macOS Toomoss）
preset/       硬件驱动与 UDS 客户端的便捷组合
```

## 安装

```bash
go get github.com/LoveWonYoung/linbuskit
```

项目当前 `go.mod` 使用 Go `1.24`。

## 快速开始

### 1. 用模拟网络跑通主从诊断

下面的示例不依赖真实硬件，适合先验证通信流程：

```go
package main

import (
	"fmt"
	"time"

	"github.com/LoveWonYoung/linbuskit/tplin"
)

func main() {
	network := tplin.NewSimulatedLinNetwork()

	slaveDriver := network.CreateSlaveDriver()
	masterDriver := network.GetMasterDriver()

	slave := tplin.NewSlave(
		0x01,                   // NAD
		0x10,                   // Variant ID
		0x1234,                 // Supplier ID
		0x5678,                 // Function ID
		[]byte{1, 2, 3, 4},     // Serial Number
		slaveDriver,
	)
	slave.Run()
	defer slave.Stop()

	master := tplin.NewMaster(masterDriver)
	defer master.Close()

	respNAD, supplierID, functionID, variantID, err := master.GetSlaveProductIdentifier(
		0x1234,
		0x5678,
		0x01,
		2*time.Second,
	)
	if err != nil {
		panic(err)
	}

	fmt.Printf(
		"respNAD=0x%02X supplierID=0x%04X functionID=0x%04X variantID=0x%02X\n",
		respNAD, supplierID, functionID, variantID,
	)
}
```

### 2. 直接发送 UDS 请求

如果你只关心诊断请求/响应，而不想自己处理 TP 细节，可以直接用 `uds_client.Client`：

```go
package main

import (
	"fmt"
	"time"

	"github.com/LoveWonYoung/linbuskit/tplin"
	"github.com/LoveWonYoung/linbuskit/uds_client"
)

func main() {
	network := tplin.NewSimulatedLinNetwork()

	slaveDriver := network.CreateSlaveDriver()
	masterDriver := network.GetMasterDriver()

	slave := tplin.NewSlave(0x01, 0x10, 0x1234, 0x5678, []byte{1, 2, 3, 4}, slaveDriver)
	slave.Run()
	defer slave.Stop()

	client := uds_client.NewClient(masterDriver, 0x01)
	defer client.Close()

	responseNAD, resp, err := client.SendAndRec([]byte{0xB2, 0x00, 0x34, 0x12, 0x78, 0x56}, 2*time.Second)
	if err != nil {
		panic(err)
	}

	fmt.Printf("response NAD=0x%02X: % X\n", responseNAD, resp)
}
```

说明：

- 第一个返回值是实际响应节点的 NAD，广播请求或同一通道存在多个节点时可据此识别响应来源
- `payload[0]` 是 SID
- `payload[1:]` 是服务数据
- 返回结果包含完整响应字节流，即 `响应 SID + 响应数据`
- 若收到负响应，返回值中仍会包含 `0x7F ...` 原始响应数据，便于上层继续处理
- 默认不自动重试，避免超时后重复执行非幂等诊断服务

只应为确认可安全重复的 SID 显式开启超时重试：

```go
config := uds_client.DefaultClientConfig(0x01)
config.MaxRetries = 2
config.RetryableSIDs = map[byte]bool{
	0x22: true, // ReadDataByIdentifier
}
client := uds_client.NewClientWithConfig(masterDriver, config)
```

### 3. 使用硬件 preset

如果使用默认的硬件主站配置，可以通过 `preset` 同时创建设备和绑定到目标 NAD、逻辑通道的 UDS 客户端。下面是 Windows 下的 PCAN/PLIN 示例：

```go
package main

import (
	"log"
	"time"

	"github.com/LoveWonYoung/linbuskit/preset"
)

func main() {
	p, err := preset.NewPresetPCAN(0x01, 0)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			log.Printf("close LIN preset: %v", err)
		}
	}()

	responseNAD, response, err := p.Request([]byte{0x22, 0xF1, 0x89}, 2*time.Second)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("response NAD=0x%02X: % X", responseNAD, response)
}
```

Windows 还提供 `NewPresetVector`、`NewPresetTSMaster` 和 `NewPresetToomoss`；macOS（Darwin + cgo）提供 `NewPresetToomoss`。`FunctionRequest` 使用广播 NAD `0x7F`，并返回实际响应节点的 NAD。`Write` 可在同一通道发送最多 8 字节的原始主站帧；`MasterRead(frameID)` 与 UDS 请求串行执行，请求结束的 transport 同步屏障会确保空闲接收循环已经退出。preset 拥有底层硬件驱动，`Close` 会先停止 UDS transport，再关闭设备；不要在 preset 活跃期间直接从 `LinDevice` 读取事件，否则会绕过 preset 的请求串行保护。

## 核心能力

### `liniface`

定义统一驱动接口：

```go
type Driver interface {
	ReadEvent(timeout time.Duration, channel Channel) (*LinEvent, error)
	WriteMessage(event *LinEvent, channel Channel) error
	ScheduleSlaveResponse(event *LinEvent, channel Channel) error
	RequestSlaveResponse(frameID byte, channel Channel) error
}
```

只要实现这组接口，就可以把任意 LIN 适配器接入 `tplin` 和 `uds_client`。

PCAN、Vector、TSMaster 和 Toomoss 硬件驱动还实现了可选的 `liniface.MasterReader`，可通过 `MasterRead(frameID, channel)` 发送主站 header 并同步取得从站 payload。PCAN、Vector 和 TSMaster 最多等待 100 ms；Toomoss 使用厂商同步 API。返回切片由调用者持有。`MasterRead` 会直接消费驱动接收事件，不应与同一通道上的 `tplin.Transport`、`uds_client.Client` 或其它 `ReadEvent` 调用并发使用。

`tplin.NewMaster(driver, channel)`、`tplin.NewSlave(..., driver, channel)` 可将实例绑定到指定通道；省略 `channel` 时使用通道 `0`。`uds_client.ClientConfig.Channel` 提供同样的选择能力。

### 设备日志

所有 `driver` 设备日志默认关闭，并由同一个进程级开关控制。需要查看 DLL 加载、初始化、收发帧、Header、无响应、队列溢出和关闭日志时，应在创建设备前启用：

```go
driver.SetPrintLog(true)
defer driver.SetPrintLog(false)
```

帧日志统一使用以下字段与格式：

```text
[PCAN] LIN direction=RX channel=0 id=0x3D length=8 checksum=0xA5 data=01 02 03 04 05 06 07 08
```

设备可能是 `PCAN`、`TOOMOSS`、`TSMASTER` 或 `VECTOR`。未调用 `SetPrintLog(true)` 时，设备驱动不会直接向标准日志输出内容；操作失败仍通过返回值或错误通道报告。

### `tplin`

提供 LIN 诊断传输层和主从站能力：

- 诊断帧 ID 使用 `0x3C`（Master Request）和 `0x3D`（Slave Response）
- 支持单帧 `SF`、首帧 `FF`、连续帧 `CF`
- 支持多帧重组和接收超时清理
- 默认配置下，主站空闲时不读取驱动；停止等待 `0x3D` 会同步等待当前读取周期退出
- `LinMaster` 已封装：
  - `AssignSlaveNad`
  - `ReadByIdentifier`
  - `GetSlaveProductIdentifier`
  - `GetSlaveSerialNumber`
  - `SendDiagnostic`
  - `ReceiveDiagnostic`
  - `ReceiveDiagnosticWithContext`
- `LinSlave` 当前已实现/处理：
  - `ReadByIdentifier (0xB2)`
  - `AssignNad (0xB0)`
  - `SaveConfiguration (0xB6)`

### `uds_client`

面向单个目标 NAD 的更高层诊断客户端：

- 默认配置通过 `DefaultClientConfig(targetNad)` 创建
- 支持超时控制和 `context.Context`
- 自动处理正响应 SID（`requestSID + 0x40`）
- 自动识别负响应 `0x7F`
- 收到 `NRC 0x78`（Response Pending）后切换到可配置的 `ResponsePendingTimeout`（P2*，默认 5 秒）
- 默认禁用自动重试；`MaxRetries` 只对 `RetryableSIDs` 中显式允许的 SID 生效

## 真实硬件接入

### Windows / macOS: Toomoss

仓库中的 `driver.NewToomoss()` 提供 Toomoss LIN 设备接入。Windows 使用 DLL；macOS 实现要求 Darwin、cgo 和对应动态库。

```go
package main

import (
	"log"
	"time"

	"github.com/LoveWonYoung/linbuskit/driver"
	"github.com/LoveWonYoung/linbuskit/uds_client"
)

func main() {
	dev, err := driver.NewToomoss(
		[]driver.ToomossCh{driver.CH1, driver.CH2},
		driver.Master,
	)
	if err != nil {
		log.Fatal(err)
	}

	config := uds_client.DefaultClientConfig(0x01)
	config.Channel = driver.CH2
	client := uds_client.NewClientWithConfig(dev, config)
	defer client.Close()

	responseNAD, resp, err := client.SendAndRec([]byte{0x22, 0xF1, 0x89}, 2*time.Second)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("response NAD=0x%02X: % X", responseNAD, resp)
}
```

注意：

- `driver/toomoss.go` 仅在 Windows 下参与编译；`driver/toomoss_darwin.go` 仅在 Darwin 且启用 cgo 时参与编译
- 同一设备只创建一个 `Toomoss` 实例，并在构造时一次传入所有需要使用的 LIN 通道
- Windows 会尝试从注册表或默认路径加载 `USB2XXX.dll` / `libusb-1.0.dll`
- 运行前需要确认对应平台的 Toomoss 驱动和动态库已正确安装

### Windows: PEAK PCAN / PLIN

`driver.NewPCAN()` 使用 PEAK 的 `PLinApi.dll` 接入 PCAN-USB Pro、PCAN-USB Pro FD 或 PLIN-USB。默认配置为主站、19200 baud、逻辑通道 0：

```go
driver.SetPrintLog(true) // 全局启用所有设备驱动日志

dev, err := driver.NewPCAN()
if err != nil {
	log.Fatal(err)
}
defer dev.Close()

config := uds_client.DefaultClientConfig(0x01)
client := uds_client.NewClientWithConfig(dev, config)
defer client.Close()
```

需要从站模式、其它波特率或多个通道时，可使用配置构造函数：

```go
dev, err := driver.NewPCANWithConfig(driver.PCANConfig{
	ClientName: "my-lin-client",
	Mode:       driver.PCANSlave,
	Baudrate:   19200,
	Channels:   []liniface.Channel{0, 1},
})
```

未设置 `HardwareHandles` 时，逻辑通道号按 `LIN_GetAvailableHardware` 的枚举顺序选择硬件；多设备环境可通过 `HardwareHandles` 显式绑定 PLIN 硬件句柄。DLL 会依次从注册表、Windows 系统目录、`./bin` 和系统 DLL 搜索路径加载。

### Windows: Vector XL LIN

`driver.NewVector()` 使用 Vector XL Driver Library 接入 LIN。下面示例直接选择 VN1640（`XL_HWTYPE_VN1640 = 59`）的硬件通道 0，并以 LIN 2.1、19200 baud 主站模式启动：

```go
dev, err := driver.NewVector(59, 0)
if err != nil {
	log.Fatal(err)
}
defer dev.Close()

config := uds_client.DefaultClientConfig(0x01)
client := uds_client.NewClientWithConfig(dev, config)
defer client.Close()
```

如果通道已经在 Vector Hardware Config 中分配给应用，建议按应用名和应用通道映射：

```go
dev, err := driver.NewVectorWithConfig(driver.VectorConfig{
	AppName:      "xl",
	UseAppConfig: true,
	Channels:     []liniface.Channel{0, 1},
	Mode:         driver.VectorLINMaster,
	Baudrate:     19200,
	Version:      driver.VectorLINVersion21,
	DLC:          map[byte]byte{0x22: 4}, // 未配置的 ID 默认 DLC=8
})
```

从站模式使用 `VectorLINSlave`。驱动实现了主站报文发送、从站响应 header 请求、从站响应预置和 LIN 事件接收，并额外提供 `WakeUp`、`SetSleepMode`、`FlushReceiveQueue`、`ReceiveQueueLevel`。诊断帧 ID `0x3C/0x3D` 固定使用经典校验；其它 ID 默认使用增强校验，可通过 `Checksum` 覆盖。DLL 会依次从注册表、Windows 系统目录、`./bin` 和系统 DLL 搜索路径加载，也可通过 `DLLPath` 指定。

### Windows: TSMaster

`driver.NewTSMaster()` 通过 TSMaster DLL 接入支持的 LIN 设备。构造参数是仓库中定义的设备类型和逻辑通道：

```go
dev, err := driver.NewTSMaster(driver.TL1001, 0)
if err != nil {
	log.Fatal(err)
}
defer dev.Close()

config := uds_client.DefaultClientConfig(0x01)
client := uds_client.NewClientWithConfig(dev, config)
defer client.Close()
```

运行前需要安装匹配架构的 TSMaster 和设备驱动，并确认目标设备类型、应用通道映射及 LIN 波特率配置正确。

### 无硬件模拟

不连接真实设备时，所有平台都可以使用：

- `tplin.SimulatedLinNetwork` 做总线级联调

Linux 当前没有仓库内置的真实硬件驱动；macOS 可在 Darwin + cgo 环境使用 Toomoss。

## 测试

运行全部测试：

```bash
go test ./...
```

当前测试覆盖了：

- 单帧发送与接收
- 多帧发送与重组
- 大报文重组
- UDS 请求、超时、负响应处理
- 生命周期幂等、关闭唤醒、通道隔离和事件所有权
- 显式 SID 超时重试策略

## 适用场景

- LIN 从节点诊断联调
- UDS over LIN 通信验证
- 产测工具或刷写工具中的诊断通道封装
- 无硬件条件下的诊断流程模拟与自动化测试

## 已知边界

- 当前从站实现的诊断服务是基础子集，不是完整 LIN 诊断规范实现
- `uds_client.Client` 目前提供的是通用收发能力，不内置完整 UDS 服务封装
- Toomoss、PCAN/PLIN、Vector XL LIN、TSMaster 驱动为平台相关实现，实际可用性取决于本机动态库、驱动和设备环境

## License

MIT，见 [LICENSE](./LICENSE)。
