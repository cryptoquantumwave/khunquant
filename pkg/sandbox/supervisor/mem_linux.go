//go:build linux

package supervisor

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
)

// FreeMemory is the memory a new process can use without swapping: MemAvailable.
func FreeMemory() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return memAvailable(f)
}

func memAvailable(r io.Reader) (uint64, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "MemAvailable:"); ok {
			fields := strings.Fields(rest) // "<n> kB"
			if len(fields) == 0 {
				break
			}
			kb, err := strconv.ParseUint(fields[0], 10, 64)
			return kb << 10, err
		}
	}
	return 0, errors.New("supervisor: MemAvailable not found in /proc/meminfo")
}
