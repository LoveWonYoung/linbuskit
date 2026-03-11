package tplin

import (
	"bytes"
	"testing"
	"time"

	"github.com/LoveWonYoung/linbuskit/liniface"
)

// TestLargeMultiFrameAssembly 测试传输层处理一个需要大量连续帧的大型多帧消息。
func TestLargeMultiFrameAssembly(t *testing.T) {
	testMultiFrameAssembly(t, 500)
}

// Test4000ByteMultiFrameAssembly 测试 4000 字节大数据传输
func Test4000ByteMultiFrameAssembly(t *testing.T) {
	testMultiFrameAssembly(t, 4000)
}

// testMultiFrameAssembly 是多帧组装测试的通用实现
func testMultiFrameAssembly(t *testing.T, dataSize int) {
	// --- 1. 设置测试环境 ---
	network := NewSimulatedLinNetwork()
	driver := network.GetMasterDriver().(*SimulatedLinDriver)

	transport := NewTransport(false, driver)
	transport.Run()
	defer transport.Close()

	// --- 2. 定义测试数据 ---
	originalNad := byte(0x1A)
	originalSid := byte(0xB2)
	originalData := make([]byte, dataSize)
	for i := 0; i < dataSize; i++ {
		// 用递增的模式填充数据，便于调试
		originalData[i] = byte(i % 256)
	}
	dataLen := len(originalData)

	t.Logf("Testing with a large payload of %d bytes.", dataLen)

	// --- 3. 手动构建并模拟接收所有帧 ---
	// 注意：长度字段应该是 SID(1字节) + 数据长度
	totalLen := dataLen + 1 // SID + Data

	// a) 构建第一帧 (First Frame - FF)
	ffPayload := make([]byte, 8)
	for i := range ffPayload {
		ffPayload[i] = 0xFF
	}
	ffPayload[0] = originalNad
	ffPayload[1] = (byte(liniface.FF) << 4) | byte(totalLen>>8&0x0F)
	ffPayload[2] = byte(totalLen & 0xFF)
	ffPayload[3] = originalSid
	copy(ffPayload[4:], originalData[0:4])

	ffEvent := &liniface.LinEvent{
		EventID:      SlaveDiagnosticFrameID,
		EventPayload: ffPayload,
		Direction:    liniface.RX,
	}

	// 将第一帧推入队列
	driver.eventQueue <- ffEvent
	t.Logf("Injected First Frame (FF).")

	// b) 循环构建并注入所有连续帧 (Consecutive Frames - CF)
	bytesSent := 4
	frameCounter := 0
	for bytesSent < dataLen {
		frameCounter = (frameCounter + 1) % 16
		cfPayload := make([]byte, 8)
		for i := range cfPayload {
			cfPayload[i] = 0xFF
		}
		cfPayload[0] = originalNad
		cfPayload[1] = (byte(liniface.CF) << 4) | byte(frameCounter)

		// 计算这次要发送的数据量
		chunkEnd := bytesSent + 6
		if chunkEnd > dataLen {
			chunkEnd = dataLen
		}
		copy(cfPayload[2:], originalData[bytesSent:chunkEnd])

		cfEvent := &liniface.LinEvent{
			EventID:      SlaveDiagnosticFrameID,
			EventPayload: cfPayload,
			Direction:    liniface.RX,
		}
		// 将连续帧推入队列
		driver.eventQueue <- cfEvent

		bytesSent = chunkEnd
	}
	t.Logf("Injected a total of %d Consecutive Frames.", frameCounter)

	// --- 4. 执行测试 ---
	// 等待足够长的时间让 transport 处理所有帧
	// 对于大数据需要更长时间
	waitTime := time.Duration(dataSize/100+100) * time.Millisecond
	time.Sleep(waitTime)

	reassembledMsg := transport.Receive()

	// --- 5. 验证结果 ---
	if reassembledMsg == nil {
		t.Fatal("Test Failed: Transport layer did not reassemble any message.")
	}

	t.Logf("成功组装出消息 (Successfully reassembled message):")
	t.Logf("  -> NAD: 0x%02X", reassembledMsg.NAD)
	t.Logf("  -> SID: 0x%02X", reassembledMsg.SID)
	t.Logf("  -> Data Length: %d bytes", len(reassembledMsg.Data))

	if reassembledMsg.NAD != originalNad {
		t.Errorf("Test Failed: NAD mismatch. Expected 0x%X, Got 0x%X", originalNad, reassembledMsg.NAD)
	}

	if reassembledMsg.SID != originalSid {
		t.Errorf("Test Failed: SID mismatch. Expected 0x%X, Got 0x%X", originalSid, reassembledMsg.SID)
	}

	if !bytes.Equal(reassembledMsg.Data, originalData) {
		t.Errorf("Test Failed: Reassembled data does not match original data.")
		// 打印前几个和最后几个字节用于调试
		if len(reassembledMsg.Data) > 10 {
			t.Logf("  Expected first 10: % 02X", originalData[:10])
			t.Logf("  Got first 10:      % 02X", reassembledMsg.Data[:10])
			t.Logf("  Expected last 10:  % 02X", originalData[len(originalData)-10:])
			t.Logf("  Got last 10:       % 02X", reassembledMsg.Data[len(reassembledMsg.Data)-10:])
		}
	}

	t.Logf("✅ Test Passed: Multi-frame message (%d bytes) was successfully reassembled.", dataSize)
}
