package storagecrypt

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

func TestKeyRejectsBroadAndNullWindowsACLs(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, trustee := range []string{"WD", "BU", "AU", "null"} {
		t.Run(trustee, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "storage.key")
			if err := os.WriteFile(path, bytes.Repeat([]byte{42}, 32), 0o600); err != nil {
				t.Fatal(err)
			}
			var dacl *windows.ACL
			var sd *windows.SECURITY_DESCRIPTOR
			if trustee != "null" {
				var err error
				sd, err = windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;GR;;;" + trustee + ")")
				if err != nil {
					t.Fatal(err)
				}
				dacl, _, err = sd.DACL()
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
				t.Fatal(err)
			}
			runtime.KeepAlive(sd)
			if _, err := ReadKeyFile(path); err == nil {
				t.Fatal("accepted a key with a permissive Windows DACL")
			}
		})
	}
}
