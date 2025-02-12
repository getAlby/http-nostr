package nostr

import (
	"time"

	"gorm.io/gorm"
)

func (svc *Service) StartDailyCleanup() {
	ticker := time.NewTicker(24 * time.Hour)

	svc.Wg.Add(1)
	go func() {
		defer svc.Wg.Done()
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				svc.Logger.Info("Running daily cleanup job...")
				err := svc.runCleanup()
				if err != nil {
					svc.Logger.WithError(err).Error("Daily cleanup job failed")
				} else {
					svc.Logger.Info("Daily cleanup job completed successfully")
				}

			case <-svc.Ctx.Done():
				svc.Logger.Info("Exiting daily cleanup job goroutine...")
				return
			}
		}
	}()
}

func (svc *Service) runCleanup() error {
	queries := []string{
		// delete closed subscriptions older than 2 weeks
		// note: this will cascade delete the corresponding response_events
		`DELETE FROM subscriptions
			 WHERE open = FALSE
				 AND created_at < NOW() - INTERVAL '2 weeks';`,

		// delete request_events older than 2 weeks
		// note: this will cascade delete the corresponding response_events
		`DELETE FROM request_events
			 WHERE created_at < NOW() - INTERVAL '2 weeks';`,

		// delete response_events older than 2 weeks
		// note: this deletes response_events from active subscriptions
		`DELETE FROM response_events
			 WHERE created_at < NOW() - INTERVAL '2 weeks';`,
	}

	return svc.db.Transaction(func(tx *gorm.DB) error {
		for _, q := range queries {
			if err := tx.Exec(q).Error; err != nil {
				svc.Logger.WithError(err).Errorf("Query failed: %s", q)
				return err
			}
		}
		return nil
	})
}
