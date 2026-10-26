package main

import (
	"context"
	"fmt"
)

type bootProtection interface {
	Protect(context.Context) error
	InstallProtection(context.Context, string, string) error
}

func prepareBoot(ctx context.Context, protection bootProtection, migrate bool) ([]string, error) {
	if err := protection.Protect(ctx); err != nil {
		return nil, fmt.Errorf("protecting the boot filesystem: %w", err)
	}
	if migrate {
		if err := protection.InstallProtection(ctx, "/etc/fstab", "/usr/local/sbin/glovebox-boot-maintenance"); err != nil {
			return []string{"Persistent boot protection setup failed; runtime protection remains active. Setup will retry on restart: " + err.Error()}, nil
		}
	}
	return nil, nil
}
