package gateway

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLoadGuardRSSUnreadableUnrelated(t *testing.T) {
	for _, fault := range []error{os.ErrNotExist, syscall.ESRCH, syscall.EACCES} {
		t.Run(fault.Error(), func(t *testing.T) {
			root := loadGuardFiles(t, map[string]string{
				"proc/10/status": "PPid: 1\nVmRSS: 1024 kB\n",
				"proc/11/status": "PPid: 10\nVmRSS: 2048 kB\n",
				"proc/12/status": "PPid: 1\nVmRSS: 4096 kB\n",
			})
			read := func(name string) ([]byte, error) {
				if filepath.Base(filepath.Dir(name)) == "12" {
					return nil, &os.PathError{Op: "read", Path: name, Err: fault}
				}
				return os.ReadFile(name)
			}
			if got := processTreeMiBRead(root, 10, read); got != 3 {
				t.Fatalf("unrelated unreadable process made RSS %d, want 3", got)
			}
			if got := processTreeMiBRead(root, 12, read); got != -1 {
				t.Fatalf("unreadable root RSS = %d, want unknown", got)
			}
		})
	}
}

func TestLoadGuardRSSZombie(t *testing.T) {
	root := loadGuardFiles(t, map[string]string{
		"proc/10/status": "PPid: 1\nVmRSS: 1024 kB\n",
		"proc/11/status": "State: Z (zombie)\nPPid: 10\n",
		"proc/12/status": "PPid: 11\nVmRSS: 2048 kB\n",
	})
	if got := processTreeMiB(root, 10); got != 3 {
		t.Fatalf("zombie made tree RSS %d, want 3", got)
	}
	if got := processTreeMiB(root, 11); got != 2 {
		t.Fatalf("zombie root with child RSS %d, want 2", got)
	}
}

func TestLoadGuardRSSMalformedAndIOError(t *testing.T) {
	root := loadGuardFiles(t, map[string]string{
		"proc/10/status": "PPid: 1\nVmRSS: 1024 kB\n",
		"proc/11/status": "PPid: 10\nVmRSS: bad kB\n",
	})
	if got := processTreeMiB(root, 10); got != -1 {
		t.Fatalf("malformed descendant RSS = %d, want unknown", got)
	}
	if got := processTreeMiBRead(root, 10, func(string) ([]byte, error) { return nil, errors.New("I/O failure") }); got != -1 {
		t.Fatalf("I/O failure RSS = %d, want unknown", got)
	}
}
