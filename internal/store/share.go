package store

// Согласие члена корпорации (этап 5 плана кабинета, ARCHITECTURE.md
// «Корпорация»).
//
// ESI не отдаёт руководству корпорации ни навыки, ни производственные
// линии её членов — таких эндпоинтов нет вовсе. Всё, что видит CEO в
// разделе «Корпорация», человек показал сам: вошёл производственным
// пресетом и тем самым согласился делиться. Согласие — строка в
// `char_share`; снимается галочкой в настройках своего кабинета.

import (
	"database/sql"
	"strings"
	"time"
)

// Блоки, которыми можно делиться. Список короткий и растёт вместе с
// разделом «Корпорация»: неизвестный блок при чтении просто ничего не
// показывает.
const (
	ShareSkills = "skills" // производственные навыки
	ShareLines  = "lines"  // производственные и исследовательские линии
)

// CharShare — согласие одного персонажа.
type CharShare struct {
	CharacterID   int64
	CorporationID int64 // корпорация на момент согласия
	Blocks        []string
	CreatedAt     time.Time
}

// Has — входит ли блок в согласие.
func (s CharShare) Has(block string) bool {
	for _, b := range s.Blocks {
		if b == block {
			return true
		}
	}
	return false
}

// splitBlocks разбирает хранимый список «через запятую».
func splitBlocks(v string) []string {
	var out []string
	for _, b := range strings.Split(v, ",") {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return out
}

// CharShare — согласие персонажа (nil, если его нет).
func (s *Store) CharShare(charID int64) (*CharShare, error) {
	var sh CharShare
	var blocks string
	var created int64
	err := s.db.QueryRow(`
SELECT character_id, corporation_id, blocks, created_at FROM char_share WHERE character_id = ?`,
		charID).Scan(&sh.CharacterID, &sh.CorporationID, &blocks, &created)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sh.Blocks = splitBlocks(blocks)
	sh.CreatedAt = time.Unix(created, 0)
	return &sh, nil
}

// SetCharShare записывает (или обновляет) согласие. Дата согласия не
// переписывается: человек согласился один раз, дальше меняется только
// корпорация и состав блоков.
func (s *Store) SetCharShare(charID, corpID int64, blocks []string) error {
	_, err := s.db.Exec(`
INSERT INTO char_share (character_id, corporation_id, blocks, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(character_id) DO UPDATE SET corporation_id = excluded.corporation_id,
                                        blocks = excluded.blocks`,
		charID, corpID, strings.Join(blocks, ","), time.Now().Unix())
	return err
}

// DeleteCharShare отзывает согласие.
func (s *Store) DeleteCharShare(charID int64) error {
	_, err := s.db.Exec(`DELETE FROM char_share WHERE character_id = ?`, charID)
	return err
}

// CharSharesForCorp — все согласия, выданные этой корпорации. Членство
// проверяет уже вызывающий: корпорация в записи — та, что была на
// момент согласия, а человек мог уйти (и вернуться).
func (s *Store) CharSharesForCorp(corpID int64) ([]CharShare, error) {
	rows, err := s.db.Query(`
SELECT character_id, corporation_id, blocks, created_at
FROM char_share WHERE corporation_id = ? ORDER BY character_id`, corpID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CharShare
	for rows.Next() {
		var sh CharShare
		var blocks string
		var created int64
		if err := rows.Scan(&sh.CharacterID, &sh.CorporationID, &blocks, &created); err != nil {
			return nil, err
		}
		sh.Blocks = splitBlocks(blocks)
		sh.CreatedAt = time.Unix(created, 0)
		out = append(out, sh)
	}
	return out, rows.Err()
}
