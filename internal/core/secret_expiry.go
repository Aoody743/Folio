package core

import (
	"context"
	"time"
)

// Only sealed expiry rows inserted with their credential can trigger deletion.
// This internal maintenance operation cannot be invoked through REST or MCP.
func (s *Service) expireProviderSecrets(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM provider_secrets WHERE ref IN (SELECT ref FROM provider_secret_expiry WHERE expires_at <= ?)", now.UnixMilli())
	return err
}

func (s *Service) runSecretJanitor() {
	defer close(s.secretJanitorDone)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.lifetime.Done():
			return
		case now := <-ticker.C:
			ctx, cancel := context.WithTimeout(s.lifetime, 5*time.Second)
			_ = s.expireProviderSecrets(ctx, now)
			cancel()
		}
	}
}
