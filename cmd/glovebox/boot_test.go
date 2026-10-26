package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type fakeBootProtection struct {
	calls        []string
	protectError error
	installError error
}

func (protection *fakeBootProtection) Protect(context.Context) error {
	protection.calls = append(protection.calls, "protect")
	return protection.protectError
}

func (protection *fakeBootProtection) InstallProtection(_ context.Context, fstab, helper string) error {
	protection.calls = append(protection.calls, "install "+fstab+" "+helper)
	return protection.installError
}

func TestBootPreparationForWebUpgrade(t *testing.T) {
	protection := &fakeBootProtection{}
	warnings, err := prepareBoot(context.Background(), protection, true)
	want := []string{"protect", "install /etc/fstab /usr/local/sbin/glovebox-boot-maintenance"}
	if err != nil || len(warnings) != 0 || !slices.Equal(protection.calls, want) {
		t.Fatalf("warnings=%q err=%v calls=%q", warnings, err, protection.calls)
	}
}

func TestBootMigrationFailureKeepsStartupAvailableAndRetries(t *testing.T) {
	protection := &fakeBootProtection{installError: errors.New("root is full")}
	warnings, err := prepareBoot(context.Background(), protection, true)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "root is full") {
		t.Fatalf("warnings=%q err=%v", warnings, err)
	}
	protection.installError = nil
	warnings, err = prepareBoot(context.Background(), protection, true)
	if err != nil || len(warnings) != 0 || len(protection.calls) != 4 {
		t.Fatalf("retry warnings=%q err=%v calls=%q", warnings, err, protection.calls)
	}
}

func TestBootProtectionFailureBlocksMigration(t *testing.T) {
	failure := errors.New("cannot remount boot")
	protection := &fakeBootProtection{protectError: failure}
	warnings, err := prepareBoot(context.Background(), protection, true)
	if !errors.Is(err, failure) || len(warnings) != 0 || !slices.Equal(protection.calls, []string{"protect"}) {
		t.Fatalf("warnings=%q err=%v calls=%q", warnings, err, protection.calls)
	}
}

func TestDevelopmentInstanceSkipsPersistentMigration(t *testing.T) {
	protection := &fakeBootProtection{}
	warnings, err := prepareBoot(context.Background(), protection, false)
	if err != nil || len(warnings) != 0 || !slices.Equal(protection.calls, []string{"protect"}) {
		t.Fatalf("warnings=%q err=%v calls=%q", warnings, err, protection.calls)
	}
}
