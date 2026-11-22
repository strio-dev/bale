package transport

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openLayoutDirectory(path string) (*os.File, error) {
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := dir.Stat()
	if err == nil && !info.IsDir() {
		err = &os.PathError{Op: "open", Path: path, Err: fmt.Errorf("not a directory")}
	}
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	return dir, nil
}

// Keep the no-reparse policy. The pinned os.Root implementation uses a
// synchronous Windows open by default, unlike the Unix nonblocking member lookup.
func openLayoutMemberRoot(string, *os.File) (*layoutMemberRoot, error) { return nil, nil }

func openLayoutMemberAt(parent *os.File, name string, directory bool) (*os.File, error) {
	// Each lookup has only a leaf: open the reparse point itself, never its
	// target. Reject all reparse points using the resulting handle.
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attrs := windows.OBJECT_ATTRIBUTES{
		RootDirectory: windows.Handle(parent.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, windows.FILE_GENERIC_READ, &attrs, &windows.IO_STATUS_BLOCK{},
		nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN, windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	runtime.KeepAlive(parent)
	if status, ok := err.(windows.NTStatus); ok {
		return nil, status.Errno()
	}
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(handle, &info)
	if err == nil && (info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		(directory && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0)) {
		err = fmt.Errorf("%w: reparse point or invalid directory component", ErrIntegrity)
	}
	if err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	return os.NewFile(uintptr(handle), name), nil
}
