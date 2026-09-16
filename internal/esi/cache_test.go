package esi

// Ключ кэша корпоративных ручек (этап 5 плана кабинета).
//
// Ответ `/corporations/{id}/…` зависит от ролей персонажа, чьим токеном
// его спросили: пока ключом был голый URL, кабинет без роли получал из
// `esi_cache` кошельки корпорации, добытые чужим токеном.

import "testing"

func TestCacheKeySplitsCorpRoutesPerCharacter(t *testing.T) {
	const (
		corpWallets = baseURL + "/corporations/1/wallets/"
		corpPublic  = baseURL + "/corporations/1/"
		charWallet  = baseURL + "/characters/5/wallet/"
	)

	// Авторизованная корпоративная ручка: у каждого персонажа своя запись.
	keys := map[string]bool{}
	for _, charID := range []int64{5, 6} {
		keys[cacheKey(charID, corpWallets)] = true
	}
	if len(keys) != 2 {
		t.Errorf("два персонажа на %s дали %d записей, ждали 2: %v", corpWallets, len(keys), keys)
	}

	// Публичная карточка корпорации (characterID = 0) — одна запись на всех.
	keys = map[string]bool{}
	for range []int{1, 2} {
		keys[cacheKey(0, corpPublic)] = true
	}
	if len(keys) != 1 {
		t.Errorf("публичная %s дала %d записей, ждали 1: %v", corpPublic, len(keys), keys)
	}
	if got := cacheKey(0, corpPublic); got != corpPublic {
		t.Errorf("публичный ключ = %q, ждали сам URL", got)
	}

	// Персональные ручки персонажа уже содержат в пути: ключ не трогаем.
	if got := cacheKey(5, charWallet); got != charWallet {
		t.Errorf("ключ персональной ручки = %q, ждали сам URL", got)
	}

	// В сеть уходит настоящий адрес: рефетч снимает хвост персонажа.
	if got := keyURL(cacheKey(6, corpWallets)); got != corpWallets {
		t.Errorf("keyURL = %q, ждали %q", got, corpWallets)
	}
	if got := keyURL(charWallet); got != charWallet {
		t.Errorf("keyURL без хвоста = %q, ждали %q", got, charWallet)
	}
}

// Ключ остаётся разбираемым для ESI Refresher: kindOf смотрит путь, а
// хвост персонажа — фрагмент URL и в путь не попадает.
func TestKindOfIgnoresCacheKeySuffix(t *testing.T) {
	url := baseURL + "/corporations/1/wallets/"
	if plain, keyed := kindOf(url), kindOf(cacheKey(7, url)); plain != keyed {
		t.Errorf("kindOf ключа = %q, у URL = %q", keyed.Name, plain.Name)
	}
}
