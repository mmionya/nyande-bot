package bot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type mediaProcessFixture struct {
	Kind string
	Text string
	Fail bool
}

// Reuse the native Go test executable as a subprocess fixture on every OS.
// A distinct executable name keeps normal test runs out of this entry point.
func init() {
	if !strings.HasPrefix(filepath.Base(os.Args[0]), "nyande-media-fixture-") {
		return
	}
	if err := runMediaProcessFixture(os.Args[0], os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runMediaProcessFixture(path string, args []string) error {
	data, err := os.ReadFile(path + ".fixture")
	if err != nil {
		return err
	}
	var fixture mediaProcessFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		return err
	}
	if err := os.WriteFile(path+".args", []byte(strings.Join(args, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	argument := func(name string) string {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == name {
				return args[i+1]
			}
		}
		return ""
	}
	switch fixture.Kind {
	case "whisper":
		if len(args) == 0 || argument("--output_dir") == "" {
			return errors.New("missing Whisper input or output directory")
		}
		input, err := os.ReadFile(args[0])
		if err != nil {
			return err
		}
		if err := os.WriteFile(path+".input", input, 0o600); err != nil {
			return err
		}
		if fixture.Fail {
			return errors.New("private recognized speech")
		}
		return os.WriteFile(filepath.Join(argument("--output_dir"), "input.txt"), []byte(fixture.Text), 0o600)
	case "probe":
		_, err := fmt.Fprint(os.Stdout, fixture.Text)
		return err
	case "encoder", "normalizer":
		calls, err := os.OpenFile(path+".calls", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := calls.WriteString("called")
		closeErr := calls.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return err
		}
		if fixture.Fail {
			return errors.New("private-token connection timeout at /private/video.mp4")
		}
		if len(args) == 0 {
			return errors.New("missing encoder output")
		}
		output := []byte(fixture.Text)
		if fixture.Kind == "normalizer" {
			input, err := os.ReadFile(argument("-i"))
			if err != nil {
				return err
			}
			output = append([]byte("normalized:"), input...)
		}
		return os.WriteFile(args[len(args)-1], output, 0o600)
	default:
		return fmt.Errorf("unknown fixture kind %q", fixture.Kind)
	}
}

func fakeMediaProcess(t *testing.T, fixture mediaProcessFixture) string {
	t.Helper()
	name := "nyande-media-fixture-" + fixture.Kind
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Windows keeps the running test executable locked, including its hard links.
	// Copy it so the fixture can be removed while the parent test is still running.
	if runtime.GOOS == "windows" || os.Link(executable, path) != nil {
		source, err := os.Open(executable)
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		destination, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.Copy(destination, source)
		if err := errors.Join(copyErr, destination.Close()); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".fixture", data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
