package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadDoor32_Full(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "DOOR32.SYS")
	body := "2\n42\n0\nELEBBS\n1\nReal Name\nHandle\n10\n60\n1\n3\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := readDoor32(p)
	if err != nil || info == nil {
		t.Fatalf("got info=%v err=%v", info, err)
	}
	if info.Comm != door32CommTelnet || info.Handle != 42 {
		t.Errorf("comm/handle = %d/%d, want 2/42", info.Comm, info.Handle)
	}
	if info.BBSID != "ELEBBS" || info.User != "Handle" || info.Node != 3 {
		t.Errorf("metadata mismatch: %+v", info)
	}
}

func TestReadDoor32_Missing(t *testing.T) {
	info, err := readDoor32(filepath.Join(t.TempDir(), "nope"))
	if err != nil || info != nil {
		t.Errorf("missing should be (nil, nil), got (%v, %v)", info, err)
	}
}

func TestReadDoor32_Truncated(t *testing.T) {
	p := filepath.Join(t.TempDir(), "DOOR32.SYS")
	if err := os.WriteFile(p, []byte("2\n5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := readDoor32(p)
	if err != nil || info.Comm != 2 || info.Handle != 5 {
		t.Errorf("got info=%+v err=%v", info, err)
	}
}

func TestReadDoor32_BadComm(t *testing.T) {
	p := filepath.Join(t.TempDir(), "DOOR32.SYS")
	if err := os.WriteFile(p, []byte("oops\n5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readDoor32(p); err == nil {
		t.Error("expected error for non-numeric comm type")
	}
}

func TestReadDoor32_CRLF(t *testing.T) {
	// Windows BBSes write CRLF; the scanner + TrimSpace must handle it.
	p := filepath.Join(t.TempDir(), "DOOR32.SYS")
	if err := os.WriteFile(p, []byte("2\r\n7\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := readDoor32(p)
	if err != nil || info.Comm != 2 || info.Handle != 7 {
		t.Errorf("got info=%+v err=%v", info, err)
	}
}
