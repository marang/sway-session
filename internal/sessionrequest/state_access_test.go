package sessionrequest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestServiceRetainsStateAccessAcrossRestoreGap(t *testing.T) {
	service, request, _, runner := testService(t)
	checked := false
	runner.afterRestore = func() error {
		gate, err := os.OpenFile(filepath.Join(service.StateRoot, ".state-access.lock"), os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer gate.Close()
		err = unix.Flock(int(gate.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			t.Fatalf("broker restore gap permitted recovery: %v", err)
		}
		checked = true
		return nil
	}
	if _, err := service.Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("restore was not exercised")
	}
}
