package collect

import (
	"context"
	"log"
	"time"
)

// AIRAutoClose подтягивает дни AIR из собранных журналов валетов
// (daily_goal_payouts, личные и корповые) и закрывает месяц, когда
// наступил синхронизированный со шкалой игры момент обновления
// (store: air_reset_at). Порядок важен: сначала дни, потом закрытие —
// в историю должен попасть свежий счёт. ESI не трогает: прогресс AIR в
// API отсутствует, журналы уже собраны задачей wallet. Для копии с
// выключенным сбором ту же связку делает открытие страницы.
func (c *Collector) AIRAutoClose(ctx context.Context) error {
	now := time.Now().UTC()
	// Шкала AIR и её таймер — вещи кабинета (момент сброса вводят руками
	// с игрового таймера), поэтому задача обходит кабинеты по очереди.
	users, err := c.Store.Users()
	if err != nil {
		return err
	}
	for _, u := range users {
		if _, err := c.Store.AirSyncWalletDays(u.ID, now); err != nil {
			return err
		}
		closed, err := c.Store.AirAutoClose(u.ID, now)
		if err != nil {
			return err
		}
		if closed {
			log.Printf("AIR: месяц закрыт по таймеру (кабинет %d), следующее обновление %s",
				u.ID, c.Store.AirResetAt(u.ID).Format("2006-01-02 15:04"))
		}
	}
	return nil
}
