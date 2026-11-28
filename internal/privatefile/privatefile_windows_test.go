package privatefile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

func TestProtectedCreationDoesNotInheritPublicAccess(t *testing.T) {
	directory := t.TempDir()
	user, err := currentUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.String() + ")(A;OICI;GR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(directory, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	runtime.KeepAlive(sd)
	if err := MkdirAll(directory, 0o700); err == nil {
		t.Fatal("permissive existing storage directory accepted")
	}
	privateDirectory := filepath.Join(directory, "private", "nested")
	if err := MkdirAll(privateDirectory, 0o700); err != nil {
		t.Fatal("creating a private child of a shared directory", err)
	}
	child, err := os.Open(privateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if err := Check(child); err != nil {
		t.Fatal("new data directory inherited public access", err)
	}
	file, err := CreateTemp(directory, "private-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := Check(file); err != nil {
		t.Fatal("temporary file inherited permissive entries", err)
	}
	// The helper must not silently rewrite an existing directory ACL.
	parent, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := Check(parent); err == nil {
		t.Fatal("existing directory permissions were changed")
	}
}
