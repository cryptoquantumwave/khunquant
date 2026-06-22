//go:build linux || darwin

package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func serialListPorts() ([]serialPortInfo, error) {
	patterns := []string{
		"/dev/ttyS*",
		"/dev/ttyUSB*",
		"/dev/ttyACM*",
		"/dev/ttyAMA*",
		"/dev/rfcomm*",
		"/dev/tty.*",
		"/dev/cu.*",
	}

	seen := make(map[string]struct{})
	ports := make([]serialPortInfo, 0)
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			if _, ok := seen[match]; ok {
				continue
			}
			info, err := os.Stat(match)
			if err != nil || info.IsDir() {
				continue
			}
			seen[match] = struct{}{}
			ports = append(ports, serialPortInfo{
				Name: filepath.Base(match),
				Path: match,
			})
		}
	}

	sort.Slice(ports, func(i, j int) bool {
		return ports[i].Path < ports[j].Path
	})
	return ports, nil
}

func serialRead(cfg serialConfig, length int, timeout time.Duration) ([]byte, error) {
	fd, err := openAndConfigureSerialPort(cfg)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)

	buf := make([]byte, length)
	total := 0
	deadline := time.Now().Add(timeout)

	for total < length {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		n, err := pollRead(fd, buf[total:], remaining)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
		total += n
	}

	return buf[:total], nil
}

func serialWrite(cfg serialConfig, data []byte, timeout time.Duration) (int, error) {
	fd, err := openAndConfigureSerialPort(cfg)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)

	total := 0
	deadline := time.Now().Add(timeout)
	for total < len(data) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return total, fmt.Errorf("timeout while writing serial data")
		}

		n, err := pollWrite(fd, data[total:], remaining)
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, fmt.Errorf("serial port accepted zero bytes")
		}
		total += n
	}

	return total, nil
}

func openAndConfigureSerialPort(cfg serialConfig) (int, error) {
	fd, err := unix.Open(normalizeUnixSerialPath(cfg.Port), unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return -1, err
	}

	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return -1, err
	}

	if err := configureUnixSerialPort(fd, cfg); err != nil {
		unix.Close(fd)
		return -1, err
	}

	return fd, nil
}

func configureUnixSerialPort(fd int, cfg serialConfig) error {
	tio, err := serialGetTermios(fd)
	if err != nil {
		return err
	}

	tio.Iflag = 0
	tio.Oflag = 0
	tio.Lflag = 0
	tio.Cflag = unix.CREAD | unix.CLOCAL
	tio.Cc[unix.VMIN] = 0
	tio.Cc[unix.VTIME] = 0

	switch cfg.DataBits {
	case 5:
		tio.Cflag |= unix.CS5
	case 6:
		tio.Cflag |= unix.CS6
	case 7:
		tio.Cflag |= unix.CS7
	default:
		tio.Cflag |= unix.CS8
	}

	switch cfg.Parity {
	case "even":
		tio.Cflag |= unix.PARENB
	case "odd":
		tio.Cflag |= unix.PARENB | unix.PARODD
	}

	if cfg.StopBits == 2 {
		tio.Cflag |= unix.CSTOPB
	}

	speed, err := serialBaudToUnix(cfg.Baud)
	if err != nil {
		return err
	}
	if err := serialSetSpeed(tio, speed); err != nil {
		return err
	}

	return serialSetTermios(fd, tio)
}

func serialBaudToUnix(baud int) (uint32, error) {
	switch baud {
	case 50:
		return unix.B50, nil
	case 75:
		return unix.B75, nil
	case 110:
		return unix.B110, nil
	case 134:
		return unix.B134, nil
	case 150:
		return unix.B150, nil
	case 200:
		return unix.B200, nil
	case 300:
		return unix.B300, nil
	case 600:
		return unix.B600, nil
	case 1200:
		return unix.B1200, nil
	case 1800:
		return unix.B1800, nil
	case 2400:
		return unix.B2400, nil
	case 4800:
		return unix.B4800, nil
	case 9600:
		return unix.B9600, nil
	case 19200:
		return unix.B19200, nil
	case 38400:
		return unix.B38400, nil
	case 57600:
		return unix.B57600, nil
	case 115200:
		return unix.B115200, nil
	case 230400:
		return unix.B230400, nil
	default:
		return 0, fmt.Errorf("unsupported baud rate on this platform: %d", baud)
	}
}

func pollRead(fd int, dst []byte, timeout time.Duration) (int, error) {
	pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(pfd, durationToPollTimeout(timeout))
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	return unix.Read(fd, dst)
}

func pollWrite(fd int, src []byte, timeout time.Duration) (int, error) {
	pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
	n, err := unix.Poll(pfd, durationToPollTimeout(timeout))
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	return unix.Write(fd, src)
}

func durationToPollTimeout(timeout time.Duration) int {
	if timeout <= 0 {
		return 0
	}
	ms := int(timeout / time.Millisecond)
	if ms == 0 {
		return 1
	}
	return ms
}

func normalizeUnixSerialPath(port string) string {
	trimmed := strings.TrimSpace(port)
	if strings.HasPrefix(trimmed, "/dev/") {
		return trimmed
	}
	return "/dev/" + trimmed
}
