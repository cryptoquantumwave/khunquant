//go:build linux

package supervisor

import (
	"strings"
	"testing"
)

func TestMemAvailable_ParsesMeminfo(t *testing.T) {
	got, err := memAvailable(strings.NewReader("MemTotal:       16384000 kB\nMemFree:          100000 kB\nMemAvailable:    8192000 kB\nBuffers: 1 kB\n"))
	if err != nil || got != 8192000<<10 {
		t.Errorf("memAvailable = %d, %v, want 8192000 kB in bytes", got, err)
	}
	if _, err := memAvailable(strings.NewReader("MemTotal: 1 kB\n")); err == nil {
		t.Error("a meminfo without MemAvailable must be an error")
	}
	if _, err := memAvailable(strings.NewReader("MemAvailable: lots kB\n")); err == nil {
		t.Error("an unreadable MemAvailable must be an error")
	}
}
