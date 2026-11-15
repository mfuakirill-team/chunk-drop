//go:build windows

package upload

import (
	"time"

	"golang.org/x/sys/windows"
)

// setBirthTime stamps the file's creation time too. Windows Explorer
// shows "Date created" prominently next to "Date modified", and a
// freshly renamed file would otherwise claim the photo was created
// today (os.Chtimes can only set access/modification times). This is
// the Windows half of the DEVICE-TESTS "phone mtime" check.
func setBirthTime(path string, t time.Time) error {
	ft := windows.NsecToFiletime(t.UnixNano())
	h, err := windows.CreateFile(
		windows.StringToUTF16Ptr(path),
		windows.FILE_WRITE_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.SetFileTime(h, &ft, nil, nil)
}
