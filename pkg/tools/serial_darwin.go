//go:build darwin

package tools

import "golang.org/x/sys/unix"

func serialGetTermios(fd int) (*unix.Termios, error) {
	return unix.IoctlGetTermios(fd, unix.TIOCGETA)
}

func serialSetSpeed(tio *unix.Termios, speed uint32) error {
	// On darwin (this x/sys version) Termios.Ispeed/Ospeed are uint64.
	tio.Ispeed = uint64(speed)
	tio.Ospeed = uint64(speed)
	return nil
}

func serialSetTermios(fd int, tio *unix.Termios) error {
	return unix.IoctlSetTermios(fd, unix.TIOCSETA, tio)
}
