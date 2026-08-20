package main

import (
	"log"
	"net/http"
	_ "time/tzdata" // в контейнере нет /usr/share/zoneinfo: TZ иначе молча = UTC

	"eve-empire/internal/config"
	"eve-empire/internal/esi"
	"eve-empire/internal/sde"
	"eve-empire/internal/sso"
	"eve-empire/internal/store"
	"eve-empire/internal/web"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	st, err := store.Open(cfg.DBPath, cfg.EncryptionKey)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	ssoClient := sso.New(cfg.ClientID, cfg.ClientSecret, cfg.CallbackURL, cfg.Scopes, cfg.UserAgent)
	esiClient := esi.New(ssoClient, st, cfg.UserAgent)
	esiClient.SetLanguage(st.Setting("language"))

	sdeDB := sde.Open(cfg.SDEPath)
	defer sdeDB.Close()
	if sdeDB.Available() {
		log.Printf("статическая база: %s (build %s)", cfg.SDEPath, sdeDB.Meta("build"))
	} else {
		log.Printf("статическая база %s не найдена — импланты без бонусов (запусти sdeimport)", cfg.SDEPath)
	}

	srv, err := web.New(ssoClient, esiClient, st, sdeDB)
	if err != nil {
		log.Fatalf("web: %v", err)
	}

	log.Printf("EVE Empire listening on %s (callback %s)", cfg.ListenAddr, cfg.CallbackURL)
	if err := http.ListenAndServe(cfg.ListenAddr, srv.Routes()); err != nil {
		log.Fatal(err)
	}
}
