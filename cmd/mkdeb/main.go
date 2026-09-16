// mkdeb собирает .deb-пакет EVE Empire без dpkg-deb, fakeroot и ar — их на
// Windows-машине нет, а формат простой: ar-архив из трёх членов
// (debian-binary, control.tar.gz, data.tar.gz).
//
//	go run ./cmd/mkdeb -bin dist/linux-amd64/eve-empire -sdeimport dist/linux-amd64/sdeimport \
//	    -version 0.1.30 -arch amd64 -assets deploy/debian -out dist/release/eve-empire_0.1.30_amd64.deb
//
// Раскладка в системе: бинарники в /usr/bin, unit в /lib/systemd/system,
// настройки (секреты SSO) в /etc/eve-empire/eve-empire.env (conffile — dpkg
// не затирает его при обновлении), базы в /var/lib/eve-empire. Скрипты
// сопровождения, unit и шаблон настроек лежат в -assets, а не зашиты сюда:
// их правят чаще, чем формат.
//
// ГРАБЛЯ: на Windows git с autocrlf отдаёт assets с CRLF, а dpkg выполняет
// postinst через /bin/sh — «#!/bin/sh\r» не найдётся. Все текстовые assets
// нормализуются к LF здесь, чтобы не зависеть от настроек checkout.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Фиксированная дата, как в mkimage: один и тот же бинарник — побайтово
// одинаковый пакет.
var epoch = time.Unix(0, 0).UTC()

type entry struct {
	name string // путь внутри data.tar без ведущего "./"
	data []byte
	mode int64
	dir  bool
}

func main() {
	bin := flag.String("bin", "", "linux-бинарник сервера (обязателен)")
	sdeimport := flag.String("sdeimport", "", "linux-бинарник sdeimport (необязателен)")
	version := flag.String("version", "", "версия пакета, например 0.1.30 (обязательна)")
	arch := flag.String("arch", "amd64", "архитектура Debian: amd64 или arm64")
	assets := flag.String("assets", "deploy/debian", "каталог с control, скриптами, unit и настройками")
	out := flag.String("out", "", "куда писать .deb (обязателен)")
	flag.Parse()
	if *bin == "" || *version == "" || *out == "" {
		log.Fatal("нужны -bin, -version и -out")
	}

	binData := mustRead(*bin)
	files := []entry{
		{name: "usr/bin/eve-empire", data: binData, mode: 0o755},
		{name: "lib/systemd/system/eve-empire.service", data: asset(*assets, "eve-empire.service"), mode: 0o644},
		{name: "etc/eve-empire/eve-empire.env", data: asset(*assets, "eve-empire.env"), mode: 0o640},
		{name: "usr/share/doc/eve-empire/SETUP.md", data: asset(*assets, "SETUP.md"), mode: 0o644},
		{name: "usr/share/doc/eve-empire/copyright", data: asset(*assets, "copyright"), mode: 0o644},
	}
	if *sdeimport != "" {
		files = append(files, entry{name: "usr/bin/eve-sdeimport", data: mustRead(*sdeimport), mode: 0o755})
	}

	// Каталоги: все родители файлов плюс /var/lib/eve-empire, который
	// postinst отдаёт пользователю службы.
	dirs := map[string]bool{"var/lib/eve-empire": true}
	for _, f := range files {
		for d := filepath.ToSlash(filepath.Dir(f.name)); d != "." && d != "/"; d = filepath.ToSlash(filepath.Dir(d)) {
			dirs[d] = true
		}
	}
	var all []entry
	for d := range dirs {
		all = append(all, entry{name: d, mode: 0o755, dir: true})
	}
	all = append(all, files...)
	sort.Slice(all, func(i, j int) bool { return all[i].name < all[j].name })

	var installed int64
	var md5sums bytes.Buffer
	for _, f := range files {
		installed += int64(len(f.data))
		md5sums.WriteString(fmt.Sprintf("%x  %s\n", md5.Sum(f.data), f.name))
	}

	dataTar := buildDataTar(all)

	control := string(asset(*assets, "control"))
	control = strings.NewReplacer(
		"${VERSION}", *version,
		"${ARCH}", *arch,
		"${SIZE}", fmt.Sprint((installed+1023)/1024),
	).Replace(control)
	if !strings.HasSuffix(control, "\n") {
		control += "\n"
	}

	ctrl := []entry{
		{name: "control", data: []byte(control), mode: 0o644},
		{name: "conffiles", data: []byte("/etc/eve-empire/eve-empire.env\n"), mode: 0o644},
		{name: "md5sums", data: md5sums.Bytes(), mode: 0o644},
	}
	for _, s := range []string{"preinst", "postinst", "prerm", "postrm"} {
		if p := filepath.Join(*assets, s); exists(p) {
			ctrl = append(ctrl, entry{name: s, data: asset(*assets, s), mode: 0o755})
		}
	}
	controlTar := buildDataTar(ctrl)

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(*out)
	if err != nil {
		log.Fatalf("вывод: %v", err)
	}
	defer f.Close()

	// ar: глобальный заголовок и три члена строго в этом порядке —
	// dpkg проверяет, что debian-binary идёт первым.
	w := io.Writer(f)
	io.WriteString(w, "!<arch>\n")
	arMember(w, "debian-binary", []byte("2.0\n"))
	arMember(w, "control.tar.gz", gz(controlTar))
	arMember(w, "data.tar.gz", gz(dataTar))

	st, _ := f.Stat()
	fmt.Printf("%s\n  пакет:  eve-empire %s (%s)\n  файлов: %d, установлено %.1f МБ\n  размер: %.1f МБ\n",
		*out, *version, *arch, len(files), float64(installed)/(1<<20), float64(st.Size())/(1<<20))
}

// buildDataTar пишет tar в раскладке dpkg: имена с "./", владелец root.
func buildDataTar(entries []entry) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	must(tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir, Name: "./", Mode: 0o755, ModTime: epoch,
		Uname: "root", Gname: "root", Format: tar.FormatGNU,
	}))
	for _, e := range entries {
		h := &tar.Header{
			Name:    "./" + e.name,
			Mode:    e.mode,
			ModTime: epoch,
			Uname:   "root",
			Gname:   "root",
			Format:  tar.FormatGNU,
		}
		if e.dir {
			h.Typeflag = tar.TypeDir
			h.Name += "/"
			must(tw.WriteHeader(h))
			continue
		}
		h.Typeflag = tar.TypeReg
		h.Size = int64(len(e.data))
		must(tw.WriteHeader(h))
		_, err := tw.Write(e.data)
		must(err)
	}
	must(tw.Close())
	return buf.Bytes()
}

func gz(data []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zw.ModTime = epoch
	_, err := zw.Write(data)
	must(err)
	must(zw.Close())
	return buf.Bytes()
}

// arMember пишет член ar-архива: заголовок 60 байт фиксированной ширины,
// тело, паддинг до чётной длины.
func arMember(w io.Writer, name string, data []byte) {
	fmt.Fprintf(w, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, epoch.Unix(), 0, 0, "100644", len(data))
	_, err := w.Write(data)
	must(err)
	if len(data)%2 == 1 {
		io.WriteString(w, "\n")
	}
}

// asset читает файл из каталога assets и приводит переводы строк к LF.
func asset(dir, name string) []byte {
	data := mustRead(filepath.Join(dir, name))
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}) // BOM от PowerShell/редакторов
	return bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
}

func mustRead(p string) []byte {
	data, err := os.ReadFile(p)
	if err != nil {
		log.Fatalf("%s: %v", p, err)
	}
	return data
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
