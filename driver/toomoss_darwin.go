//go:build darwin && cgo

package driver

/*
#include <dlfcn.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct _LIN_EX_MSG{
    unsigned int  Timestamp;    ///<从机接收数据时代表时间戳，单位为ms;主机读写数据时，表示数据读写后的延时时间，单位为ms
    unsigned char MsgType;      ///<帧类型
    unsigned char CheckType;    ///<校验类型
    unsigned char DataLen;      ///<LIN数据段有效数据字节数
    unsigned char Sync;         ///<固定值，0x55
    unsigned char PID;          ///<帧ID，发送数据填入ID即可，接收数据时为PID，ID=PID&0x3F
    unsigned char Data[8];      ///<数据，有效数据通过DataLen来获取
    unsigned char Check;        ///<校验,只有校验数据类型为LIN_EX_CHECK_USER的时候才需要用户传入数据
    unsigned char BreakBits;    ///<该帧的BRAK信号位数，有效值为10到26，若设置为其他值则默认为13位
    unsigned char Reserve1;     ///<保留
}LIN_EX_MSG,*PLIN_EX_MSG;


typedef int (*fn_USB_ScanDevice)(int* pDevHandle);
typedef bool (*fn_USB_OpenDevice)(int DevHandle);
typedef bool (*fn_USB_CloseDevice)(int DevHandle);

typedef int (*fn_LIN_EX_Init)(int DevHandle,unsigned char LINIndex,unsigned int BaudRate,unsigned char MasterMode);
typedef int (*fn_LIN_EX_MasterSync)(int DevHandle,unsigned char LINIndex,LIN_EX_MSG *pInMsg,LIN_EX_MSG *pOutMsg,unsigned int MsgLen);
typedef int (*fn_LIN_EX_SlaveGetData)(int DevHandle,unsigned char LINIndex,LIN_EX_MSG *pLINMsg);

static void* g_libusb = NULL;
static void* g_usb2xxx = NULL;

static fn_USB_ScanDevice pUSB_ScanDevice = NULL;
static fn_USB_OpenDevice pUSB_OpenDevice = NULL;
static fn_USB_CloseDevice pUSB_CloseDevice = NULL;
static fn_LIN_EX_Init pLIN_EX_Init = NULL;
static fn_LIN_EX_MasterSync pLIN_EX_MasterSync = NULL;
static fn_LIN_EX_SlaveGetData pLIN_EX_SlaveGetData = NULL;

void lin_toomoss_unload();

static int write_error(char* errbuf, size_t errlen, const char* prefix, const char* detail) {
	if (errbuf != NULL && errlen > 0) {
		if (detail == NULL) {
			snprintf(errbuf, errlen, "%s", prefix);
		} else {
			snprintf(errbuf, errlen, "%s: %s", prefix, detail);
		}
	}
	return -1;
}

#define LOAD_SYMBOL(dst, type, name, errbuf, errlen) \
	do { \
		dlerror(); \
		dst = (type)dlsym(g_usb2xxx, name); \
		const char* sym_err = dlerror(); \
		if (sym_err != NULL || dst == NULL) { \
			lin_toomoss_unload(); \
			return write_error(errbuf, errlen, name, sym_err); \
		} \
	} while (0)

int lin_toomoss_load(const char* libusb_path, const char* usb2xxx_path, char* errbuf, size_t errlen) {
	if (g_usb2xxx != NULL) {
		return 0;
	}

	if (errbuf != NULL && errlen > 0) {
		errbuf[0] = '\0';
	}

	g_libusb = dlopen(libusb_path, RTLD_NOW | RTLD_GLOBAL);
	if (g_libusb == NULL) {
		return write_error(errbuf, errlen, "dlopen libusb-1.0.0.dylib failed", dlerror());
	}

	g_usb2xxx = dlopen(usb2xxx_path, RTLD_NOW | RTLD_GLOBAL);
	if (g_usb2xxx == NULL) {
		const char* err = dlerror();
		dlclose(g_libusb);
		g_libusb = NULL;
		return write_error(errbuf, errlen, "dlopen libUSB2XXX.dylib failed", err);
	}

	LOAD_SYMBOL(pUSB_ScanDevice, fn_USB_ScanDevice, "USB_ScanDevice", errbuf, errlen);
	LOAD_SYMBOL(pUSB_OpenDevice, fn_USB_OpenDevice, "USB_OpenDevice", errbuf, errlen);
	LOAD_SYMBOL(pUSB_CloseDevice, fn_USB_CloseDevice, "USB_CloseDevice", errbuf, errlen);
	LOAD_SYMBOL(pLIN_EX_Init, fn_LIN_EX_Init, "LIN_EX_Init", errbuf, errlen);
	LOAD_SYMBOL(pLIN_EX_MasterSync, fn_LIN_EX_MasterSync, "LIN_EX_MasterSync", errbuf, errlen);
	LOAD_SYMBOL(pLIN_EX_SlaveGetData, fn_LIN_EX_SlaveGetData, "LIN_EX_SlaveGetData", errbuf, errlen);
	return 0;
}

void lin_toomoss_unload() {
	pUSB_ScanDevice = NULL;
	pUSB_OpenDevice = NULL;
	pUSB_CloseDevice = NULL;
	pLIN_EX_Init = NULL;
	pLIN_EX_MasterSync = NULL;
	pLIN_EX_SlaveGetData = NULL;

	if (g_usb2xxx != NULL) {
		dlclose(g_usb2xxx);
		g_usb2xxx = NULL;
	}
	if (g_libusb != NULL) {
		dlclose(g_libusb);
		g_libusb = NULL;
	}
}

int lin_toomoss_usb_scan_device(int* pDevHandle) {
	if (pUSB_ScanDevice == NULL) return -1;
	return pUSB_ScanDevice(pDevHandle);
}

int lin_toomoss_usb_open_device(int DevHandle) {
	if (pUSB_OpenDevice == NULL) return -1;
	return pUSB_OpenDevice(DevHandle) ? 1 : 0;
}

int lin_toomoss_usb_close_device(int DevHandle) {
	if (pUSB_CloseDevice == NULL) return -1;
	return pUSB_CloseDevice(DevHandle) ? 1 : 0;
}

int lin_toomoss_lin_ex_init(int DevHandle, unsigned char LINIndex, unsigned int BaudRate, unsigned char MasterMode) {
	if (pLIN_EX_Init == NULL) return -1;
	return pLIN_EX_Init(DevHandle, LINIndex, BaudRate, MasterMode);
}

int lin_toomoss_lin_ex_master_sync(int DevHandle, unsigned char LINIndex, LIN_EX_MSG *pInMsg, LIN_EX_MSG *pOutMsg, unsigned int MsgLen) {
	if (pLIN_EX_MasterSync == NULL) return -1;
	return pLIN_EX_MasterSync(DevHandle, LINIndex, pInMsg, pOutMsg, MsgLen);
}

int lin_toomoss_lin_ex_slave_get_data(int DevHandle, unsigned char LINIndex, LIN_EX_MSG *pLINMsg) {
	if (pLIN_EX_SlaveGetData == NULL) return -1;
	return pLIN_EX_SlaveGetData(DevHandle, LINIndex, pLINMsg);
}
*/
import "C"
import (
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/LoveWonYoung/linbuskit/liniface"
	"github.com/LoveWonYoung/linbuskit/tplin"
)

const (
	toomossLibusbPath = "/Applications/TCANLINPro.app/Contents/Frameworks/libusb-1.0.0.dylib"
	toomossUSB2XXX    = "/Applications/TCANLINPro.app/Contents/Frameworks/libUSB2XXX.dylib"
)

const (
	LIN_EX_SUCCESS = -iota
	LIN_EX_ERR_NOT_SUPPORT
	LIN_EX_ERR_USB_WRITE_FAIL
	LIN_EX_ERR_USB_READ_FAIL
	LIN_EX_ERR_CMD_FAIL
	LIN_EX_ERR_CH_NO_INIT
	LIN_EX_ERR_READ_DATA
	LIN_EX_ERR_PARAMETER
)

const (
	LIN_EX_MSG_TYPE_UN = iota
	LIN_EX_MSG_TYPE_MW
	LIN_EX_MSG_TYPE_MR
	LIN_EX_MSG_TYPE_SW
	LIN_EX_MSG_TYPE_SR
	LIN_EX_MSG_TYPE_BK
	LIN_EX_MSG_TYPE_SY
	LIN_EX_MSG_TYPE_ID
	LIN_EX_MSG_TYPE_DT
	LIN_EX_MSG_TYPE_CK
	LIN_EX_CHECK_STD   = iota - 10 // 标准校验，不含PID
	LIN_EX_CHECK_EXT               // 增强校验，含PID
	LIN_EX_CHECK_USER              // 自定义校验类型，需要用户自行计算并传入Check，不进行自动校验
	LIN_EX_CHECK_NONE              // 不进行校验数据
	LIN_EX_CHECK_ERROR             // 接收数据校验错误
)

type LinExMsg struct {
	Timestamp uint32
	MsgType   uint8
	CheckType uint8
	DataLen   uint8
	Sync      uint8
	PID       uint8
	Data      [8]uint8
	Check     uint8
	BreakBits uint8
	Reserve1  uint8
}

type ToomossCh = liniface.Channel

const (
	CH1 ToomossCh = iota
	CH2
	CH3
	CH4
)

var (
	Bt        uint = 19200
	Master    byte = 1
	SlaveMode byte = 0

	DevHandle [10]C.int
	DEVIndex  = 0

	toomossMu             sync.Mutex
	toomossLoaded         bool
	toomossInstanceMu     sync.Mutex
	toomossInstanceActive bool
)

type Toomoss struct {
	callMu     sync.Mutex
	stateMu    sync.RWMutex
	closeOnce  sync.Once
	closed     bool
	channels   map[liniface.Channel]struct{}
	eventMu    sync.Mutex
	eventChans map[liniface.Channel]chan *liniface.LinEvent
}

func resetToomossState() {
	DevHandle = [10]C.int{}
	toomossLoaded = false
}

func ensureToomossLoaded() error {
	toomossMu.Lock()
	defer toomossMu.Unlock()

	if toomossLoaded {
		return nil
	}

	libusbPath := C.CString(toomossLibusbPath)
	defer C.free(unsafe.Pointer(libusbPath))

	usb2xxxPath := C.CString(toomossUSB2XXX)
	defer C.free(unsafe.Pointer(usb2xxxPath))

	var errBuf [512]C.char
	if ret := C.lin_toomoss_load(libusbPath, usb2xxxPath, &errBuf[0], C.size_t(len(errBuf))); ret != 0 {
		return fmt.Errorf("load Toomoss dylib failed: %s", C.GoString(&errBuf[0]))
	}

	toomossLoaded = true
	return nil
}

func usbScan() (bool, error) {
	if err := ensureToomossLoaded(); err != nil {
		return false, err
	}

	ret := int(C.lin_toomoss_usb_scan_device(&DevHandle[DEVIndex]))
	if ret <= 0 {
		return false, nil
	}
	return true, nil
}

func UsbScan() bool {
	ok, err := usbScan()
	if err != nil {
		logDriverf(logDeviceToomoss, "usb_scan status=failed error=%v", err)
		return false
	}
	return ok
}

func usbOpen() (bool, error) {
	if err := ensureToomossLoaded(); err != nil {
		return false, err
	}

	stateValue := int(C.lin_toomoss_usb_open_device(DevHandle[DEVIndex]))
	return stateValue >= 1, nil
}

func UsbOpen() bool {
	ok, err := usbOpen()
	if err != nil {
		logDriverf(logDeviceToomoss, "usb_open status=failed error=%v", err)
		return false
	}
	return ok
}

func usbClose() error {
	toomossMu.Lock()
	defer toomossMu.Unlock()

	if !toomossLoaded {
		return nil
	}

	ret := int(C.lin_toomoss_usb_close_device(DevHandle[DEVIndex]))
	if ret < 1 {
		return fmt.Errorf("USB_CloseDevice returned %d", ret)
	}

	C.lin_toomoss_unload()
	resetToomossState()
	return nil
}

func ensureLinReady() error {
	if err := ensureToomossLoaded(); err != nil {
		return fmt.Errorf("load Toomoss LIN dylibs: %w", err)
	}
	return nil
}

func goToCLINMsg(msg LinExMsg) C.LIN_EX_MSG {
	var cMsg C.LIN_EX_MSG
	cMsg.Timestamp = C.uint(msg.Timestamp)
	cMsg.MsgType = C.uchar(msg.MsgType)
	cMsg.CheckType = C.uchar(msg.CheckType)
	cMsg.DataLen = C.uchar(msg.DataLen)
	cMsg.Sync = C.uchar(msg.Sync)
	cMsg.PID = C.uchar(msg.PID)
	for i := 0; i < 8; i++ {
		cMsg.Data[i] = C.uchar(msg.Data[i])
	}
	cMsg.Check = C.uchar(msg.Check)
	cMsg.BreakBits = C.uchar(msg.BreakBits)
	cMsg.Reserve1 = C.uchar(msg.Reserve1)
	return cMsg
}

func cToGoLINMsg(cMsg C.LIN_EX_MSG) LinExMsg {
	var msg LinExMsg
	msg.Timestamp = uint32(cMsg.Timestamp)
	msg.MsgType = uint8(cMsg.MsgType)
	msg.CheckType = uint8(cMsg.CheckType)
	msg.DataLen = uint8(cMsg.DataLen)
	msg.Sync = uint8(cMsg.Sync)
	msg.PID = uint8(cMsg.PID)
	for i := 0; i < 8; i++ {
		msg.Data[i] = uint8(cMsg.Data[i])
	}
	msg.Check = uint8(cMsg.Check)
	msg.BreakBits = uint8(cMsg.BreakBits)
	msg.Reserve1 = uint8(cMsg.Reserve1)
	return msg
}

func NewToomoss(channels []ToomossCh, mode byte) (*Toomoss, error) {
	toomossInstanceMu.Lock()
	defer toomossInstanceMu.Unlock()
	if toomossInstanceActive {
		return nil, errors.New("a Toomoss device instance is already active; configure all LIN channels on that instance")
	}
	if len(channels) == 0 {
		return nil, errors.New("at least one LIN channel is required")
	}
	if err := ensureLinReady(); err != nil {
		return nil, err
	}
	if ok := UsbScan(); !ok {
		return nil, fmt.Errorf("USB scan failed: device not found or dylib missing")
	}
	if ok := UsbOpen(); !ok {
		return nil, fmt.Errorf("USB open failed")
	}

	for _, channel := range channels {
		ret := C.lin_toomoss_lin_ex_init(
			DevHandle[DEVIndex],
			C.uchar(channel),
			C.uint(Bt),
			C.uchar(mode),
		)
		if ret != 0 {
			_ = usbClose()
			return nil, fmt.Errorf("failed to initialize Toomoss LIN channel %d: ret=%d", channel, int(ret))
		}
	}

	logDriverf(logDeviceToomoss, "initialized channels=%v mode=%d baudrate=%d", channels, mode, Bt)

	initializedChannels := make(map[liniface.Channel]struct{}, len(channels))
	for _, channel := range channels {
		initializedChannels[channel] = struct{}{}
	}
	toomossInstanceActive = true
	return &Toomoss{
		channels:   initializedChannels,
		eventChans: make(map[liniface.Channel]chan *liniface.LinEvent),
	}, nil
}

func (d *Toomoss) LinMasterSync(msg, outMsg []LinExMsg, channel ToomossCh) (uintptr, error) {
	if len(outMsg) == 0 || len(msg) == 0 {
		return 0, fmt.Errorf("LinMasterSync called with empty outMsg")
	}
	if len(msg) != len(outMsg) {
		return 0, fmt.Errorf("LinMasterSync: len(msg) != len(outMsg)")
	}

	cIn := make([]C.LIN_EX_MSG, len(msg))
	cOut := make([]C.LIN_EX_MSG, len(outMsg))
	for i := range msg {
		cIn[i] = goToCLINMsg(msg[i])
	}

	d.callMu.Lock()
	defer d.callMu.Unlock()
	if err := d.validateChannel(channel); err != nil {
		return 0, err
	}
	ret := C.lin_toomoss_lin_ex_master_sync(
		DevHandle[DEVIndex],
		C.uchar(channel),
		&cIn[0],
		&cOut[0],
		C.uint(len(msg)),
	)

	for i := range outMsg {
		outMsg[i] = cToGoLINMsg(cOut[i])
	}
	return uintptr(ret), nil
}

func (d *Toomoss) ReadEvent(timeout time.Duration, channel liniface.Channel) (*liniface.LinEvent, error) {
	if err := d.validateChannel(channel); err != nil {
		return nil, err
	}
	eventChan := d.eventChannel(channel)
	deadline := time.Now().Add(timeout)
	for {
		select {
		case event := <-eventChan:
			return event, nil
		default:
		}
		messages, err := d.LinExSlaveGetData(channel)
		if err != nil {
			return nil, err
		}
		if len(messages) > 0 {
			for i := 1; i < len(messages); i++ {
				select {
				case eventChan <- toomossEvent(messages[i], channel):
				default:
				}
			}
			return toomossEvent(messages[0], channel), nil
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			return nil, nil
		}
		time.Sleep(time.Millisecond)
	}
}

func toomossEvent(message LinExMsg, channel liniface.Channel) *liniface.LinEvent {
	dataLen := min(int(message.DataLen), len(message.Data))
	payload := append([]byte(nil), message.Data[:dataLen]...)
	direction := liniface.RX
	if message.MsgType == LIN_EX_MSG_TYPE_SW {
		direction = liniface.TX
	}
	checksumType := liniface.EnhancedChecksum
	if message.CheckType == LIN_EX_CHECK_STD {
		checksumType = liniface.ClassicChecksum
	}
	return &liniface.LinEvent{
		Channel:      channel,
		EventID:      message.PID & 0x3F,
		EventPayload: payload,
		ChecksumType: checksumType,
		Direction:    direction,
		Timestamp:    time.Now(),
	}
}

func (d *Toomoss) WriteMessage(event *liniface.LinEvent, channel liniface.Channel) error {
	if event == nil {
		return errors.New("nil LIN event")
	}
	if len(event.EventPayload) > 8 {
		return fmt.Errorf("invalid LIN payload length %d (max 8)", len(event.EventPayload))
	}
	msg := make([]LinExMsg, 1)
	outMsg := make([]LinExMsg, 1)
	var payload [8]byte
	copy(payload[:], event.EventPayload)

	msg[0].MsgType = LIN_EX_MSG_TYPE_MW
	msg[0].DataLen = uint8(len(event.EventPayload))
	msg[0].PID = event.EventID
	msg[0].Data = payload
	if event.EventID == tplin.MasterDiagnosticFrameID || event.EventID == tplin.SlaveDiagnosticFrameID {
		msg[0].CheckType = LIN_EX_CHECK_STD
	} else {
		msg[0].CheckType = LIN_EX_CHECK_EXT
	}

	ret, err := d.LinMasterSync(msg, outMsg, channel)
	if ret <= 0 {
		return fmt.Errorf("toomoss LIN write failed: ret=%d, err=%v", ret, err)
	}
	logLINMessage(logDeviceToomoss, "TX", channel, event.EventID, outMsg[0].Check, payload[:outMsg[0].DataLen])
	txEvent := *event
	txEvent.EventPayload = append([]byte(nil), event.EventPayload...)
	txEvent.Channel = channel
	txEvent.Direction = liniface.TX
	txEvent.Timestamp = time.Now()

	select {
	case d.eventChannel(channel) <- &txEvent:
	default:
		logDriverf(logDeviceToomoss, "queue_overflow channel=%d id=0x%02X action=drop", channel, event.EventID)
	}
	return nil
}

func (d *Toomoss) MasterWrite(frameID byte, data []byte, channel ToomossCh) error {
	if len(data) > 8 {
		return fmt.Errorf("toomoss MasterWrite: data length %d exceeds 8", len(data))
	}

	msg := make([]LinExMsg, 1)
	outMsg := make([]LinExMsg, 1)
	var payload [8]byte
	copy(payload[:], data)

	msg[0].MsgType = LIN_EX_MSG_TYPE_MW
	msg[0].DataLen = uint8(len(data))
	msg[0].PID = frameID
	msg[0].Data = payload
	if frameID == tplin.MasterDiagnosticFrameID || frameID == tplin.SlaveDiagnosticFrameID {
		msg[0].CheckType = LIN_EX_CHECK_STD
	} else {
		msg[0].CheckType = LIN_EX_CHECK_EXT
	}
	ret, err := d.LinMasterSync(msg, outMsg, channel)

	if ret <= 0 {
		return fmt.Errorf("toomoss LIN write failed: ret=%d, err=%v", ret, err)
	}
	logLINMessage(logDeviceToomoss, "TX", liniface.Channel(channel), frameID, outMsg[0].Check, payload[:outMsg[0].DataLen])
	return nil
}

func (d *Toomoss) MasterRead(frameID byte, channel ToomossCh) ([]byte, error) {
	msg := make([]LinExMsg, 1)
	outMsg := make([]LinExMsg, 1)
	msg[0].MsgType = LIN_EX_MSG_TYPE_MR
	msg[0].PID = frameID
	ret, _ := d.LinMasterSync(msg, outMsg, channel)

	if ret <= 0 {
		return nil, errors.New("no response from slave")
	}

	dataLen := int(outMsg[0].DataLen)
	if dataLen > len(outMsg[0].Data) {
		dataLen = len(outMsg[0].Data)
	}
	result := make([]byte, dataLen)
	copy(result, outMsg[0].Data[:dataLen])
	logLINMessage(logDeviceToomoss, "RX", liniface.Channel(channel), frameID, outMsg[0].Check, outMsg[0].Data[:dataLen])
	return result, nil
}

func (d *Toomoss) RequestSlaveResponse(frameID byte, channel liniface.Channel) error {
	msg := make([]LinExMsg, 1)
	outMsg := make([]LinExMsg, 1)
	msg[0].MsgType = LIN_EX_MSG_TYPE_MR
	msg[0].PID = frameID
	ret, _ := d.LinMasterSync(msg, outMsg, channel)

	if ret <= 0 {
		logLINNoResponse(logDeviceToomoss, channel, frameID)
		return nil
	}

	responseData := outMsg[0].Data
	dataLen := outMsg[0].DataLen
	if int(dataLen) > len(responseData) {
		dataLen = byte(len(responseData))
	}
	if dataLen == 0 {
		logLINNoResponse(logDeviceToomoss, channel, frameID)
		return nil
	}
	if ret == 1 {
		logLINMessage(logDeviceToomoss, "RX", channel, frameID, outMsg[0].Check, responseData[:dataLen])
	}

	rxEvent := &liniface.LinEvent{
		Channel:      channel,
		EventID:      frameID,
		EventPayload: responseData[:dataLen],
		Direction:    liniface.RX,
		Timestamp:    time.Now(),
	}

	select {
	case d.eventChannel(channel) <- rxEvent:
	default:
		return errors.New("toomoss event channel is full, discarding slave response")
	}
	return nil
}

func (d *Toomoss) ScheduleSlaveResponse(event *liniface.LinEvent, channel liniface.Channel) error {
	return errors.New("toomoss: ScheduleSlaveResponse is not supported in Master mode")
}

func (d *Toomoss) eventChannel(channel liniface.Channel) chan *liniface.LinEvent {
	d.eventMu.Lock()
	defer d.eventMu.Unlock()
	eventChan := d.eventChans[channel]
	if eventChan == nil {
		eventChan = make(chan *liniface.LinEvent, 10)
		d.eventChans[channel] = eventChan
	}
	return eventChan
}

// Close releases the USB adapter and loaded driver library.
func (d *Toomoss) Close() error {
	if d == nil {
		return nil
	}
	var closeErr error
	d.closeOnce.Do(func() {
		d.stateMu.Lock()
		d.closed = true
		d.stateMu.Unlock()
		d.callMu.Lock()
		closeErr = usbClose()
		d.callMu.Unlock()
		if closeErr != nil {
			logDriverf(logDeviceToomoss, "disconnect status=failed error=%v", closeErr)
		} else {
			logDriverf(logDeviceToomoss, "disconnected")
		}
		toomossInstanceMu.Lock()
		toomossInstanceActive = false
		toomossInstanceMu.Unlock()
	})
	return closeErr
}

func (d *Toomoss) LinBreak(channel ToomossCh) error {
	msg := make([]LinExMsg, 1)
	outMsg := make([]LinExMsg, 1)
	msg[0].MsgType = LIN_EX_MSG_TYPE_BK
	msg[0].Timestamp = 20
	if ret, _ := d.LinMasterSync(msg, outMsg, channel); ret <= 0 {
		return errors.New("LIN break failed")
	}
	return nil
}

const linExSlaveGetDataMaxFrames = 512

func (d *Toomoss) LinExSlaveGetData(channel ToomossCh) ([]LinExMsg, error) {
	if err := d.validateChannel(channel); err != nil {
		return nil, err
	}
	if err := ensureToomossLoaded(); err != nil {
		return nil, err
	}

	cMsgs := make([]C.LIN_EX_MSG, linExSlaveGetDataMaxFrames)
	d.callMu.Lock()
	if err := d.validateChannel(channel); err != nil {
		d.callMu.Unlock()
		return nil, err
	}
	ret := int(C.lin_toomoss_lin_ex_slave_get_data(
		DevHandle[DEVIndex],
		C.uchar(channel),
		&cMsgs[0],
	))
	d.callMu.Unlock()
	if ret < 0 {
		return nil, fmt.Errorf("LIN_EX_SlaveGetData failed: ret=%d", ret)
	}

	count := ret
	if count > len(cMsgs) {
		count = len(cMsgs)
	}
	linMsgs := make([]LinExMsg, count)
	for i := 0; i < count; i++ {
		linMsgs[i] = cToGoLINMsg(cMsgs[i])
	}
	return linMsgs, nil
}

func (d *Toomoss) validateChannel(channel liniface.Channel) error {
	if d == nil {
		return liniface.ErrDriverClosed
	}
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	if d.closed {
		return liniface.ErrDriverClosed
	}
	if _, ok := d.channels[channel]; !ok {
		return fmt.Errorf("%w: %d", liniface.ErrInvalidChannel, channel)
	}
	return nil
}
