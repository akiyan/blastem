package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBlastEMSaveProfileIntegration(t *testing.T) {
	binary, fixture := os.Getenv("BLASTEM_INTEGRATION_BINARY"), os.Getenv("BLASTEM_INTEGRATION_ROM")
	if binary == "" || fixture == "" {
		t.Skip("requires patched core and SRAM fixture")
	}
	t.Setenv("SDL_AUDIODRIVER", "dummy")
	profile := t.TempDir()
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	romA := filepath.Join(t.TempDir(), "build-A.bin")
	romB := filepath.Join(t.TempDir(), "build-B.bin")
	if err := os.WriteFile(romA, data, 0o600); err != nil {
		t.Fatal(err)
	}
	data[0x1D0] ^= 1
	if err := os.WriteFile(romB, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager()
	t.Cleanup(func() { _ = manager.Close() })
	start := func(rom string) {
		t.Helper()
		status, err := manager.Start(StartConfig{BinaryPath: binary, ROMPath: rom, Debug: true, SaveDir: profile})
		if err != nil {
			t.Fatal(err)
		}
		if status.SaveDir != profile {
			t.Fatalf("save_dir = %q", status.SaveDir)
		}
	}
	start(romA)
	other := NewManager()
	if _, err := other.Start(StartConfig{BinaryPath: binary, ROMPath: romB, SaveDir: profile}); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("concurrent profile error = %v", err)
	}
	debugger, err := manager.GDB()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := debugger.WriteMemory(ctx, 0x200001, []byte{0xA5}); err != nil {
		t.Fatal(err)
	}
	controller, _, err := manager.Control()
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.FlushSRAM()
	if err != nil {
		t.Fatal(err)
	}
	if result.Bytes != 32768 || !result.Paused {
		t.Fatalf("flush = %+v", result)
	}
	if err := manager.Stop(); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(profile, "save.sram"))
	if err != nil || len(saved) != 32768 || saved[0] != 0xA5 {
		t.Fatalf("persisted SRAM length=%d error=%v", len(saved), err)
	}
	start(romB)
	debugger, _ = manager.GDB()
	restored, err := debugger.ReadMemory(ctx, 0x200001, 1)
	if err != nil || len(restored) != 1 || restored[0] != 0xA5 {
		t.Fatalf("restored SRAM = %x, %v", restored, err)
	}
	if err := manager.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestBlastEMSaveFailureOnStopIntegration(t *testing.T) {
	binary, fixture := os.Getenv("BLASTEM_INTEGRATION_BINARY"), os.Getenv("BLASTEM_INTEGRATION_ROM")
	if binary == "" || fixture == "" {
		t.Skip("requires patched core and SRAM fixture")
	}
	t.Setenv("SDL_AUDIODRIVER", "dummy")
	profile := t.TempDir()
	manager := NewManager()
	t.Cleanup(func() { _ = manager.Close() })
	if _, err := manager.Start(StartConfig{BinaryPath: binary, ROMPath: fixture, SaveDir: profile}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(profile, "save.sram"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(); err == nil || !strings.Contains(err.Error(), "io_error") {
		t.Fatalf("failed save was hidden: %v", err)
	}
	if manager.Status().Running {
		t.Fatal("session still running after failed shutdown flush")
	}
	_, lock, err := lockProfile(profile)
	if err != nil {
		t.Fatal("profile not released after failed shutdown", err)
	}
	lock.Close()
}

func TestSaveProfileLockLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile")
	_, first, err := lockProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, f, err := lockProfile(path); err == nil {
		f.Close()
		t.Fatal("duplicate profile accepted")
	}
	first.Close()
	_, reopened, err := lockProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
	if _, f, err := lockProfile("relative"); err == nil {
		f.Close()
		t.Fatal("relative profile accepted")
	}
}
