package privatefile

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func currentUser() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid, nil
}

func trustedSID(sid, user *windows.SID) bool {
	return sid != nil && (sid.Equals(user) || sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
}

func checkDescriptor(sd *windows.SECURITY_DESCRIPTOR, user *windows.SID, directory bool) error {
	if sd == nil {
		return ErrPermissions
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if !trustedSID(owner, user) {
		return ErrPermissions
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		// A missing or null DACL allows everyone; an allocated empty DACL does not.
		return ErrPermissions
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE || (!directory && ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0) {
			continue
		}
		// Fail closed on object/callback ACE layouts we do not interpret.
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return ErrPermissions
		}
		if ace.Mask == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if directory && ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 && sid.IsWellKnown(windows.WinCreatorOwnerSid) {
			continue
		}
		if !trustedSID(sid, user) {
			return ErrPermissions
		}
	}
	runtime.KeepAlive(sd)
	return nil
}

// Check validates the ACL on the opened object, not a potentially replaced path.
func Check(file *os.File) error {
	user, err := currentUser()
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return checkDescriptor(sd, user, info.IsDir())
}

func descriptor(directory bool) (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := currentUser()
	if err != nil {
		return nil, err
	}
	flags := ""
	if directory {
		flags = "OICI"
	}
	// Explicit owner and protected DACL prevent permissive inherited entries.
	return windows.SecurityDescriptorFromString("O:" + user.String() + "D:P(A;" + flags + ";FA;;;" + user.String() + ")(A;" + flags + ";FA;;;SY)(A;" + flags + ";FA;;;BA)")
}

func Protect(path string) error {
	sd, err := descriptor(false)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	runtime.KeepAlive(sd)
	return err
}

// MkdirAll creates protected directories and validates an existing data directory.
// SQLite sidecars then inherit private access even before the bot can inspect them.
// Existing directory ACLs are never silently rewritten.
func MkdirAll(path string, _ os.FileMode) error {
	path = filepath.Clean(path)
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return &os.PathError{Op: "mkdir", Path: path, Err: windows.ERROR_DIRECTORY}
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := Check(file); err != nil {
			return fmt.Errorf("private storage directory %s: %w", path, err)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(path)
	if parent != path {
		if _, err := os.Stat(parent); os.IsNotExist(err) {
			if err := MkdirAll(parent, 0); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	sd, err := descriptor(true)
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	err = windows.CreateDirectory(name, &sa)
	runtime.KeepAlive(sd)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return MkdirAll(path, 0)
	}
	return err
}

func create(path string, disposition uint32) (*os.File, error) {
	sd, err := descriptor(false)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.WRITE_DAC, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, &sa, disposition, windows.FILE_ATTRIBUTE_NORMAL, 0)
	runtime.KeepAlive(sd)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(handle), path)
	if disposition == windows.OPEN_ALWAYS {
		dacl, _, err := sd.DACL()
		if err == nil {
			err = windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
		}
		runtime.KeepAlive(sd)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return file, nil
}

func Open(path string) (*os.File, error) { return create(path, windows.OPEN_ALWAYS) }

func CreateTemp(directory, pattern string) (*os.File, error) {
	if strings.ContainsAny(pattern, `/\`) {
		return nil, errors.New("temporary file pattern contains a path separator")
	}
	prefix, suffix := pattern, ""
	if index := strings.LastIndexByte(pattern, '*'); index >= 0 {
		prefix, suffix = pattern[:index], pattern[index+1:]
	}
	for range 100 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		file, err := create(filepath.Join(directory, prefix+hex.EncodeToString(random[:])+suffix), windows.CREATE_NEW)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		return file, err
	}
	return nil, errors.New("cannot allocate a unique private temporary file")
}

// Replace publishes an already flushed file on the same volume. Unlike opening
// and syncing a directory, MoveFileEx is supported by Windows file systems.
func Replace(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return &os.LinkError{Op: "rename", Old: source, New: target, Err: err}
	}
	return nil
}
