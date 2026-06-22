//go:build !linux && !darwin && !windows

package tools

import (
	"fmt"
	"time"
)

func serialListPorts() ([]serialPortInfo, error) {
	return nil, nil
}

func serialRead(cfg serialConfig, length int, timeout time.Duration) ([]byte, error) {
	return nil, fmt.Errorf("serial is not supported on this platform")
}

func serialWrite(cfg serialConfig, data []byte, timeout time.Duration) (int, error) {
	return 0, fmt.Errorf("serial is not supported on this platform")
}
