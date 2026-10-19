package binding

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRejectsCorruptData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings.json")
	if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err == nil {
		t.Fatal("损坏的绑定文件不应被忽略")
	}
}

func TestStoreUniquenessAndTransfer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings.json")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(1, "user@example.com", "db1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(2, "user@example.com", "db1"); !errors.Is(err, ErrEmailBound) {
		t.Fatalf("应拒绝重复绑定: %v", err)
	}
	if err := s.Set(2, "user@example.com", "db2"); err != nil {
		t.Fatal(err)
	}
	old, err := s.Transfer(3, "user@example.com", "db1")
	if err != nil || old != 1 || s.Has(1) || !s.Has(3) || !s.Has(2) {
		t.Fatalf("迁移绑定异常: old=%d err=%v", old, err)
	}
	reloaded, err := New(path)
	if err != nil || reloaded.Has(1) || !reloaded.Has(2) || !reloaded.Has(3) {
		t.Fatalf("重新加载绑定异常: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("绑定文件权限不安全: %v %v", info, err)
	}
}

func TestStoreFailedSaveDoesNotChangeMemory(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "missing", "bindings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(1, "user@example.com", "db"); err == nil || s.Has(1) {
		t.Fatalf("保存失败不应修改内存: %v", err)
	}
}
