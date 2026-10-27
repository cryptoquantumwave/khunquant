//go:build darwin

package supervisor

import (
	"os"
	"syscall"
)

// FreeMemory is the free plus purgeable pages times the page size. Inactive file pages
// are reclaimable too but have no sysctl, so this errs low: a busy machine may be
// refused where it would have coped. KQ_SANDBOX_MIN_FREE_MB tunes the floor.
func FreeMemory() (uint64, error) {
	free, err := syscall.SysctlUint32("vm.page_free_count")
	if err != nil {
		return 0, err
	}
	purgeable, _ := syscall.SysctlUint32("vm.page_purgeable_count") // optional
	return (uint64(free) + uint64(purgeable)) * uint64(os.Getpagesize()), nil
}
