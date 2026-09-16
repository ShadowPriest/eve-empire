package store

// Согласие члена корпорации (этап 5 плана кабинета). Персонажи и
// корпорации выдуманы: репозиторий публичный.

import (
	"testing"
	"time"
)

func TestCharShareCRUD(t *testing.T) {
	s := testStore(t)
	const (
		charID = int64(9101)
		corpA  = int64(7001)
		corpB  = int64(7002)
	)

	if sh, err := s.CharShare(charID); err != nil || sh != nil {
		t.Fatalf("CharShare без записи = (%v, %v), ждали (nil, nil)", sh, err)
	}

	if err := s.SetCharShare(charID, corpA, []string{ShareSkills, ShareLines}); err != nil {
		t.Fatalf("SetCharShare: %v", err)
	}
	sh, err := s.CharShare(charID)
	if err != nil || sh == nil {
		t.Fatalf("CharShare: (%v, %v)", sh, err)
	}
	if sh.CorporationID != corpA || !sh.Has(ShareSkills) || !sh.Has(ShareLines) {
		t.Errorf("согласие = %+v, ждали корпорацию %d и оба блока", *sh, corpA)
	}
	if sh.CreatedAt.IsZero() {
		t.Error("дата согласия не записана")
	}

	// Повторное согласие переносит корпорацию и состав блоков.
	if err := s.SetCharShare(charID, corpB, []string{ShareLines}); err != nil {
		t.Fatalf("SetCharShare повторно: %v", err)
	}
	sh, _ = s.CharShare(charID)
	if sh.CorporationID != corpB || sh.Has(ShareSkills) || !sh.Has(ShareLines) {
		t.Errorf("после переезда согласие = %+v, ждали корпорацию %d и только lines", *sh, corpB)
	}

	// Выборка по корпорации: старая пуста, новая знает персонажа.
	if list, err := s.CharSharesForCorp(corpA); err != nil || len(list) != 0 {
		t.Errorf("CharSharesForCorp(старая) = (%v, %v), ждали пусто", list, err)
	}
	list, err := s.CharSharesForCorp(corpB)
	if err != nil || len(list) != 1 || list[0].CharacterID != charID {
		t.Fatalf("CharSharesForCorp = (%v, %v), ждали одного персонажа %d", list, err, charID)
	}

	if err := s.DeleteCharShare(charID); err != nil {
		t.Fatalf("DeleteCharShare: %v", err)
	}
	if sh, err := s.CharShare(charID); err != nil || sh != nil {
		t.Errorf("после отзыва CharShare = (%v, %v), ждали (nil, nil)", sh, err)
	}
}

// Удаление персонажа из кабинета снимает и согласие: делиться данными
// персонажа, которого в сервисе больше нет, не с кем и нечем.
func TestDeleteCharacterDropsShare(t *testing.T) {
	s := testStore(t)
	const (
		charID = int64(9102)
		corpID = int64(7003)
	)
	uid, err := s.CreateUser(true)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.UpsertCharacter(uid, charID, "Test Pilot", "rt", "at",
		time.Now().Add(time.Hour), []string{"publicData"}, "cid"); err != nil {
		t.Fatalf("UpsertCharacter: %v", err)
	}
	if err := s.SetCharShare(charID, corpID, []string{ShareSkills}); err != nil {
		t.Fatalf("SetCharShare: %v", err)
	}
	if err := s.DeleteCharacter(charID); err != nil {
		t.Fatalf("DeleteCharacter: %v", err)
	}
	if sh, err := s.CharShare(charID); err != nil || sh != nil {
		t.Errorf("после удаления персонажа CharShare = (%v, %v), ждали (nil, nil)", sh, err)
	}
	if list, err := s.CharSharesForCorp(corpID); err != nil || len(list) != 0 {
		t.Errorf("CharSharesForCorp = (%v, %v), ждали пусто", list, err)
	}
}
