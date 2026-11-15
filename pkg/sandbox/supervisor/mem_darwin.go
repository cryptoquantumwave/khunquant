//go:build darwin

package supervisor

import (
	"os"
	"syscall"
)

// FreeMemory is the memory a new process can use without swapping, as MemAvailable is
// on Linux: the free pages, the purgeable ones, and the file-backed pageable ones (the
// file cache, which the kernel drops on demand). macOS keeps few pages free on purpose
// and fills the rest with file cache, so free plus purgeable alone read a few hundred
// MiB on an idle machine with tens of GiB to give, and the floor refused it. Anonymous
// memory that could be compressed or swapped out is not counted, so this still errs
// low. KQ_SANDBOX_MIN_FREE_MB tunes the floor.
func FreeMemory() (uint64, error) {
	free, err := syscall.SysctlUint32("vm.page_free_count")
	if err != nil {
		return 0, err
	}
	purgeable, _ := syscall.SysctlUint32("vm.page_purgeable_count")          // optional
	fileBacked, _ := syscall.SysctlUint32("vm.page_pageable_external_count") // optional
	return availableBytes(free, purgeable, fileBacked, os.Getpagesize()), nil
}

// availableBytes adds the page counts as 64-bit numbers: three 32-bit counts of 16 KiB
// pages pass 4 GiB of pages long before they pass the machine's memory.
func availableBytes(free, purgeable, fileBacked uint32, pageSize int) uint64 {
	return (uint64(free) + uint64(purgeable) + uint64(fileBacked)) * uint64(pageSize)
}
