# linbuskit

`linbuskit` 是一个面向 Go 的 LIN 总线诊断工具库，覆盖了从底层驱动抽象、LIN Transport Protocol（单帧/多帧收发），到面向诊断场景的主站/从站封装，以及一个可直接使用的 UDS over LIN 客户端。

当前仓库更适合作为库集成到上位机、产线工具、诊断脚本或测试程序中，而不是一个开箱即用的 CLI。

## 特性

- 抽象统一的 LIN 驱动接口，便于接入真实硬件或模拟环境
- 内置 LIN 诊断传输层，支持单帧、首帧、连续帧的收发与重组
- 提供主站 `tplin.LinMaster`，封装常见诊断命令
- 提供从站 `tplin.LinSlave`，可模拟 ECU 节点并处理基础诊断服务
- 提供 `uds_client.Client`，用于发送 UDS 请求并处理正响应、负响应和超时
- 提供模拟网络 `tplin.SimulatedLinNetwork`，方便联调和单元测试
- Windows 下支持 Toomoss 设备驱动接入

## 包结构

```text
liniface/     底层接口定义，包含 Driver、LinEvent、校验类型等
tplin/        LIN 诊断传输层、主站/从站封装、模拟网络
uds_client/   更高层的 UDS over LIN 客户端
driver/       驱动实现（Windows Toomoss、非 Windows MockDriver）
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

	resp, err := client.SendAndRec([]byte{0xB2, 0x00, 0x34, 0x12, 0x78, 0x56}, 2*time.Second)
	if err != nil {
		panic(err)
	}

	fmt.Printf("response: % X\n", resp)
}
```

说明：

- `payload[0]` 是 SID
- `payload[1:]` 是服务数据
- 返回结果包含完整响应字节流，即 `响应 SID + 响应数据`
- 若收到负响应，返回值中仍会包含 `0x7F ...` 原始响应数据，便于上层继续处理

## 核心能力

### `liniface`

定义统一驱动接口：

```go
type Driver interface {
	ReadEvent(timeout time.Duration) (*LinEvent, error)
	WriteMessage(event *LinEvent) error
	ScheduleSlaveResponse(event *LinEvent) error
	RequestSlaveResponse(frameID byte) error
}
```

只要实现这组接口，就可以把任意 LIN 适配器接入 `tplin` 和 `uds_client`。

### `tplin`

提供 LIN 诊断传输层和主从站能力：

- 诊断帧 ID 使用 `0x3C`（Master Request）和 `0x3D`（Slave Response）
- 支持单帧 `SF`、首帧 `FF`、连续帧 `CF`
- 支持多帧重组和接收超时清理
- `LinMaster` 已封装：
  - `AssignSlaveNad`
  - `ReadByIdentifier`
  - `GetSlaveProductIdentifier`
  - `GetSlaveSerialNumber`
  - `SendDiagnostic`
  - `ReceiveDiagnostic`
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
- 会对 `NRC 0x78`（Response Pending）持续等待

## 真实硬件接入

### Windows: Toomoss

仓库中的 `driver.NewToomoss()` 提供了对 Toomoss LIN 设备的接入，适用于 Windows 环境。

```go
package main

import (
	"log"
	"time"

	"github.com/LoveWonYoung/linbuskit/driver"
	"github.com/LoveWonYoung/linbuskit/uds_client"
)

func main() {
	dev, err := driver.NewToomoss()
	if err != nil {
		log.Fatal(err)
	}

	client := uds_client.NewClient(dev, 0x01)
	defer client.Close()

	resp, err := client.SendAndRec([]byte{0x22, 0xF1, 0x89}, 2*time.Second)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("resp: % X", resp)
}
```

注意：

- `driver/toomoss.go` 仅在 Windows 下参与编译
- 代码会尝试从注册表或默认路径加载 `USB2XXX.dll` / `libusb-1.0.dll`
- 运行前需要确认 Toomoss 驱动和相关 DLL 已正确安装

### 非 Windows

非 Windows 平台下没有真实硬件驱动实现，但可以使用：

- `tplin.SimulatedLinNetwork` 做总线级联调
- `driver.MockDriver` 做更轻量的驱动侧测试

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

## 适用场景

- LIN 从节点诊断联调
- UDS over LIN 通信验证
- 产测工具或刷写工具中的诊断通道封装
- 无硬件条件下的诊断流程模拟与自动化测试

## 已知边界

- 当前从站实现的诊断服务是基础子集，不是完整 LIN 诊断规范实现
- `uds_client.Client` 目前提供的是通用收发能力，不内置完整 UDS 服务封装
- Toomoss 驱动为平台相关实现，实际可用性取决于本机 DLL 和设备环境

## License

MIT，见 [LICENSE](./LICENSE)。
