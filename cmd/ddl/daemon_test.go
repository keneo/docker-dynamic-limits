package main

import (
	"errors"
	"strings"
	"testing"
)

func TestWrapConnErr_ConnectionRefused(t *testing.T) {
	orig := errors.New("dial tcp 127.0.0.1:7123: connect: connection refused")
	wrapped := wrapConnErr(orig)
	msg := wrapped.Error()
	if !strings.Contains(msg, "connect to ddld") {
		t.Errorf("expected 'connect to ddld' in error, got: %s", msg)
	}
	if !strings.Contains(msg, `ddl daemon start`) {
		t.Errorf("expected hint in error, got: %s", msg)
	}
}

func TestWrapConnErr_ConnectionReset(t *testing.T) {
	orig := errors.New("read tcp: connection reset by peer")
	wrapped := wrapConnErr(orig)
	msg := wrapped.Error()
	if !strings.Contains(msg, `ddl daemon start`) {
		t.Errorf("expected hint in error, got: %s", msg)
	}
}

func TestWrapConnErr_OtherError(t *testing.T) {
	orig := errors.New("some other error")
	wrapped := wrapConnErr(orig)
	msg := wrapped.Error()
	if strings.Contains(msg, "Hint") {
		t.Errorf("unexpected hint in error for non-connection error, got: %s", msg)
	}
}

func TestWrapConnErr_Nil(t *testing.T) {
	if wrapConnErr(nil) != nil {
		t.Error("expected nil for nil input")
	}
}
