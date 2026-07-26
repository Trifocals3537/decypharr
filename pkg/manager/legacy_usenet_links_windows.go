//go:build windows

package manager

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func legacyUsenetLinkCount(rooted *os.Root, name string, _ os.FileInfo) (uint64, error) {
	path := filepath.Join(rooted.Name(), name)
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return 0, fmt.Errorf("open artifact for link-count inspection: %w", err)
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return 0, fmt.Errorf("read artifact link count: %w", err)
	}
	return uint64(info.NumberOfLinks), nil
}
