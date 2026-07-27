//go:build unix

package manager

import (
	"fmt"
	"os"
	"syscall"
)

func legacyUsenetLinkCount(_ *os.Root, _ string, info os.FileInfo) (uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("filesystem link count is unavailable")
	}
	return uint64(stat.Nlink), nil
}
