//go:build unix

package lock

import (
	"path/filepath"
	"testing"
)

func TestAcquireAndRelease(t *testing.T) {
	p := filepath.Join(t.TempDir(), "localchat.lock")
	l1, ok, err := Acquire(p)
	if err != nil || !ok {
		t.Fatalf("first Acquire: ok=%v err=%v", ok, err)
	}

	l2, ok2, err := Acquire(p)
	if err != nil || ok2 || l2 != nil {
		t.Fatalf("expected second Acquire blocked: ok=%v err=%v", ok2, err)
	}

	if err := l1.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	l3, ok3, err := Acquire(p)
	if err != nil || !ok3 {
		t.Fatalf("re-Acquire after Release: ok=%v err=%v", ok3, err)
	}
	_ = l3.Release()
}
