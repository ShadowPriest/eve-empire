package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // в контейнере нет /usr/share/zoneinfo: TZ иначе молча = UTC

	"eve-empire/internal/collect"
	"eve-empire/internal/config"
	"eve-empire/internal/esi"
	"eve-empire/internal/sched"
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Фоновый сбор для учёта ТМЦ: ESI отдаёт кошельки, контракты и работы
	// скользящим окном и забывает их. Дев-копия обычно ставит COLLECTOR=off,
	// чтобы две копии не дублировали трафик (ARCHITECTURE.md, «Две копии»).
	var scheduler *sched.Scheduler
	if cfg.Collector {
		scheduler = sched.New()
		for _, t := range collect.New(esiClient, st, cfg.ClientID).Tasks() {
			scheduler.Add(t)
		}
		scheduler.Start(ctx)
	} else {
		log.Print("сбор данных выключен (COLLECTOR=off)")
	}

	httpSrv := &http.Server{Addr: cfg.ListenAddr, Handler: srv.Routes()}
	go func() {
		log.Printf("EVE Empire listening on %s (callback %s)", cfg.ListenAddr, cfg.CallbackURL)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	stop() // второй Ctrl+C убивает процесс, не дожидаясь остановки
	log.Print("останавливаюсь…")

	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdown); err != nil {
		log.Printf("http: %v", err)
	}
	if scheduler != nil {
		scheduler.Stop()
	}
}
