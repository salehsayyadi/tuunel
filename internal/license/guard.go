package license

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// CheckInterval is how often a running daemon re-verifies its license.
var CheckInterval = 10 * time.Second

// Wait blocks until the installed license is valid (or ctx ends). The
// tunnel is not started meanwhile; "tuunel license sync" (systemd timer) or
// a reinstall with a new code brings a valid license.
func Wait(ctx context.Context, log *slog.Logger) error {
	last := ""
	for {
		st := Check()
		if st.Valid {
			if st.Payload != nil {
				log.Info("license valid", "id", st.Payload.ID, "role", st.Payload.Role,
					"expires", time.Unix(st.Payload.Expires, 0).UTC().Format(time.RFC3339), "left", st.Left.Round(time.Second).String())
			}
			return nil
		}
		if st.Reason != last {
			log.Error("tunnel locked: license not valid; waiting for a valid license", "reason", st.Reason)
			last = st.Reason
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(CheckInterval):
		}
	}
}

// ErrLicense is returned by the daemon when its license stops being valid.
var ErrLicense = errors.New("license no longer valid: tunnel stopped")

// Watch re-checks the license until ctx ends and calls stop once it becomes
// invalid (expired, revoked, removed).
func Watch(ctx context.Context, log *slog.Logger, stop func(error)) {
	t := time.NewTicker(CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if st := Check(); !st.Valid {
				log.Error("license no longer valid: stopping the tunnel", "reason", st.Reason)
				stop(ErrLicense)
				return
			}
		}
	}
}
