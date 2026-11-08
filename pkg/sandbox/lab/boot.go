package lab

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
)

// NextBoot counts the boots of an instance home: 1 for the first process that runs in it,
// then one more for every restart, kill or crash. The count is saved (<home>/lab/boot)
// before it is returned, so no two processes of one home share a number, and it only grows.
// A file that is not a count stops the lab: a reset would let a restarted process mint the
// ids its predecessor did.
func NextBoot(home string) (int64, error) {
	dir := filepath.Join(home, "lab")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, fmt.Errorf("lab: boot counter: %w", err)
	}
	path := filepath.Join(dir, "boot")
	var n int64
	b, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return 0, fmt.Errorf("lab: boot counter: %w", err)
	default:
		if n, err = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err != nil || n < 0 {
			return 0, fmt.Errorf("lab: boot counter %s is not a count: %q", path, b)
		}
	}
	n++
	if err := fileutil.WriteFileAtomic(path, []byte(strconv.FormatInt(n, 10)+"\n"), 0o600); err != nil {
		return 0, fmt.Errorf("lab: boot counter: %w", err)
	}
	return n, nil
}

// MixSeed gives boot number boot of a home its own stream out of the instance's seed. The
// first boot keeps the seed itself, so a scenario run on a fresh home is unchanged and two
// runs of it agree; every later boot gets a different stream, so a restarted instance does
// not mint again the approval codes, button nonces and proactive ids that are already in
// its database and journal.
func MixSeed(seed, boot int64) int64 {
	if boot <= 1 {
		return seed
	}
	z := uint64(seed) + uint64(boot)*0x9e3779b97f4a7c15 // splitmix64 over (seed, boot)
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return int64(z ^ (z >> 31))
}
