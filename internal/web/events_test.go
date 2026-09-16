package web

import (
	"encoding/json"
	"testing"
	"time"

	"eve-empire/internal/esi"
)

func TestHubRoutesChangesToDependentPages(t *testing.T) {
	reg := esi.New(nil, nil, "test").Refresher()
	h := newHub(reg)
	h.setDeps(depKey{1, "/characters/1/wallet"}, map[string]bool{"u-wallet": true, "u-online": false})
	h.setDeps(depKey{1, "/characters/2/wallet"}, map[string]bool{"u-other": true})
	// Тот же путь у ДРУГОГО кабинета — отдельный ключ и отдельные
	// зависимости: подписчик первого кабинета их видеть не должен.
	h.setDeps(depKey{2, "/characters/1/wallet"}, map[string]bool{"u-foreign": true})

	sub := h.subscribe(depKey{1, "/characters/1/wallet"})
	defer h.unsubscribe(sub)

	wallet := &esi.Kind{Title: "кошелёк"}
	online := &esi.Kind{Title: "онлайн"}
	h.changed("u-wallet", wallet)
	h.changed("u-online", online)  // sidebar dependency still notifies
	h.changed("u-other", wallet)   // another page: not for this subscriber
	h.changed("u-foreign", wallet) // чужой кабинет на том же пути — тоже мимо

	select {
	case msg := <-sub.ch:
		var ev struct{ Kinds []string }
		if err := json.Unmarshal(msg, &ev); err != nil {
			t.Fatal(err)
		}
		if len(ev.Kinds) != 2 || ev.Kinds[0] != "кошелёк" || ev.Kinds[1] != "онлайн" {
			t.Errorf("kinds %v, want [кошелёк онлайн]", ev.Kinds)
		}
	case <-time.After(eventDebounce + time.Second):
		t.Fatal("no event within the debounce window")
	}
	select {
	case msg := <-sub.ch:
		t.Fatalf("unexpected second event %s", msg)
	case <-time.After(200 * time.Millisecond):
	}

	// Only the page's own reads are watched, never the sidebar's.
	if got := reg.Snapshot().Watched; got != 1 {
		t.Errorf("watched %d, want 1", got)
	}
}
