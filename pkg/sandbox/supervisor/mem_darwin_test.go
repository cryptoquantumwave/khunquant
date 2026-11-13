//go:build darwin

package supervisor

import (
	"os"
	"syscall"
	"testing"
)

func TestAvailableBytes_CountsTheFileCache(t *testing.T) {
	// An idle 48 GiB Mac: 85 MiB free, 280 MiB purgeable, 7.6 GiB of file cache.
	const page = 16384
	if got, want := availableBytes(5433, 17675, 499656, page), uint64(5433+17675+499656)*page; got != want {
		t.Errorf("availableBytes = %d, want %d", got, want)
	}
	if got := availableBytes(5433, 17675, 0, page); got >= 1<<30 {
		t.Errorf("free plus purgeable alone = %d bytes: the case this test is about is under the 1 GiB floor", got)
	}
	if got := availableBytes(5433, 17675, 499656, page); got < 1<<30 {
		t.Errorf("with the file cache = %d bytes, want over the 1 GiB floor", got)
	}
	// No 32-bit overflow: three counts near the top of uint32.
	const big = ^uint32(0)
	if got, want := availableBytes(big, big, big, page), 3*uint64(big)*page; got != want {
		t.Errorf("availableBytes at the top of uint32 = %d, want %d", got, want)
	}
}

// On this machine: the file cache is counted. The counts move between the two reads, so
// only half of it is required.
func TestFreeMemory_ReadsTheSysctls(t *testing.T) {
	if _, err := syscall.SysctlUint32("vm.page_free_count"); err != nil {
		t.Skipf("vm.page_free_count: %v", err)
	}
	fileBacked, err := syscall.SysctlUint32("vm.page_pageable_external_count")
	if err != nil {
		t.Fatalf("vm.page_pageable_external_count is not a 32-bit sysctl here: %v; FreeMemory would fall back to free plus purgeable", err)
	}
	got, err := FreeMemory()
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(fileBacked) / 2 * uint64(os.Getpagesize()); got < want {
		t.Errorf("FreeMemory = %d bytes, under half of the file cache (%d pages): it is not counted", got, fileBacked)
	}
}
