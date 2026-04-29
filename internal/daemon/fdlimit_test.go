//go:build !windows

package daemon

import (
	"errors"
	"syscall"
	"testing"
)

func TestRaiseFDLimit_RaisesSoftCapAndDoesNotShrink(t *testing.T) {
	var before syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &before); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}

	got, err := RaiseFDLimit()
	if err != nil {
		t.Fatalf("RaiseFDLimit: %v", err)
	}
	if got.Soft < uint64(before.Cur) {
		t.Fatalf("soft cap shrank: before=%d after=%d", before.Cur, got.Soft)
	}

	var after syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &after); err != nil {
		t.Fatalf("Getrlimit (after): %v", err)
	}
	if uint64(after.Cur) != got.Soft {
		t.Fatalf("returned soft (%d) does not match observed soft (%d)", got.Soft, after.Cur)
	}
}

func TestIsEMFILE(t *testing.T) {
	if !isEMFILE(syscall.EMFILE) {
		t.Fatal("EMFILE should be detected")
	}
	if !isEMFILE(syscall.ENFILE) {
		t.Fatal("ENFILE should be detected")
	}
	wrapped := &fdErr{err: syscall.EMFILE}
	if !isEMFILE(wrapped) {
		t.Fatal("wrapped EMFILE should be detected via errors.Is")
	}
	if isEMFILE(syscall.EACCES) {
		t.Fatal("EACCES is not an FD-exhaustion error")
	}
	if isEMFILE(errors.New("boom")) {
		t.Fatal("plain error must not match")
	}
}

type fdErr struct{ err error }

func (e *fdErr) Error() string { return e.err.Error() }
func (e *fdErr) Unwrap() error { return e.err }
