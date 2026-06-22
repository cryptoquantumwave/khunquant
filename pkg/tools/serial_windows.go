//go:build windows

package tools

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	kernel32            = windows.NewLazySystemDLL("kernel32.dll")
	procGetCommState    = kernel32.NewProc("GetCommState")
	procSetCommState    = kernel32.NewProc("SetCommState")
	procSetCommTimeouts = kernel32.NewProc("SetCommTimeouts")
	procPurgeComm       = kernel32.NewProc("PurgeComm")
)

const (
	purgeTxClear = 0x0004
	purgeRxClear = 0x0008
)

type dcb struct {
	DCBlength  uint32
	BaudRate   uint32
	Flags      uint32
	Reserved   uint16
	XonLim     uint16
	XoffLim    uint16
	ByteSize   byte
	Parity     byte
	StopBits   byte
	XonChar    byte
	XoffChar   byte
	ErrorChar  byte
	EofChar    byte
	EvtChar    byte
	wReserved1 uint16
}

type commTimeouts struct {
	ReadIntervalTimeout         uint32
	ReadTotalTimeoutMultiplier  uint32
	ReadTotalTimeoutConstant    uint32
	WriteTotalTimeoutMultiplier uint32
	WriteTotalTimeoutConstant   uint32
}

func serialListPorts() ([]serialPortInfo, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DEVICEMAP\SERIALCOMM`, registry.QUERY_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return nil, nil
		}
		return nil, err
	}
	defer key.Close()

	names, err := key.ReadValueNames(-1)
	if err != nil {
		return nil, err
	}

	ports := make([]serialPortInfo, 0, len(names))
	seen := make(map[string]struct{})
	for _, name := range names {
		value, _, err := key.GetStringValue(name)
		if err != nil {
			continue
		}
		portName := strings.TrimSpace(value)
		if portName == "" {
			continue
		}
		normalized := strings.ToUpper(portName)
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		ports = append(ports, serialPortInfo{
			Name: normalized,
			Path: normalized,
		})
	}

	sort.Slice(ports, func(i, j int) bool {
		return ports[i].Path < ports[j].Path
	})
	return ports, nil
}

func serialRead(cfg serialConfig, length int, timeout time.Duration) ([]byte, error) {
	handle, err := openAndConfigureWindowsSerial(cfg, timeout)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(handle)

	buf := make([]byte, length)
	var read uint32
	if err := windows.ReadFile(handle, buf, &read, nil); err != nil {
		return nil, err
	}
	return buf[:read], nil
}

func serialWrite(cfg serialConfig, data []byte, timeout time.Duration) (int, error) {
	handle, err := openAndConfigureWindowsSerial(cfg, timeout)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(handle)

	var written uint32
	if err := windows.WriteFile(handle, data, &written, nil); err != nil {
		return int(written), err
	}
	return int(written), nil
}

func openAndConfigureWindowsSerial(cfg serialConfig, timeout time.Duration) (windows.Handle, error) {
	path := normalizeWindowsSerialPath(cfg.Port)
	handle, err := windows.CreateFile(
		windows.StringToUTF16Ptr(path),
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		return 0, err
	}

	if err := configureWindowsSerialPort(handle, cfg, timeout); err != nil {
		windows.CloseHandle(handle)
		return 0, err
	}
	return handle, nil
}

func configureWindowsSerialPort(handle windows.Handle, cfg serialConfig, timeout time.Duration) error {
	state := dcb{DCBlength: uint32(unsafe.Sizeof(dcb{}))}
	r1, _, err := procGetCommState.Call(uintptr(handle), uintptr(unsafe.Pointer(&state)))
	if r1 == 0 {
		return err
	}

	state.BaudRate = uint32(cfg.Baud)
	state.ByteSize = byte(cfg.DataBits)
	state.Flags |= 0x00000001 // fBinary

	switch cfg.Parity {
	case "even":
		state.Parity = 2
		state.Flags |= 0x00000002 // fParity
	case "odd":
		state.Parity = 1
		state.Flags |= 0x00000002 // fParity
	default:
		state.Parity = 0
		state.Flags &^= 0x00000002
	}

	switch cfg.StopBits {
	case 2:
		state.StopBits = 2
	default:
		state.StopBits = 0
	}

	r1, _, err = procSetCommState.Call(uintptr(handle), uintptr(unsafe.Pointer(&state)))
	if r1 == 0 {
		return err
	}

	timeoutMS := uint32(timeout / time.Millisecond)
	if timeoutMS == 0 {
		timeoutMS = 1
	}
	timeouts := commTimeouts{
		ReadIntervalTimeout:         timeoutMS,
		ReadTotalTimeoutConstant:    timeoutMS,
		WriteTotalTimeoutConstant:   timeoutMS,
		ReadTotalTimeoutMultiplier:  0,
		WriteTotalTimeoutMultiplier: 0,
	}
	r1, _, err = procSetCommTimeouts.Call(uintptr(handle), uintptr(unsafe.Pointer(&timeouts)))
	if r1 == 0 {
		return err
	}

	procPurgeComm.Call(uintptr(handle), uintptr(purgeRxClear|purgeTxClear))
	return nil
}

func normalizeWindowsSerialPath(port string) string {
	trimmed := strings.ToUpper(strings.TrimSpace(port))
	if strings.HasPrefix(trimmed, `\\.\`) {
		return trimmed
	}
	if filepath.VolumeName(trimmed) != "" {
		return trimmed
	}
	return `\\.\` + trimmed
}
