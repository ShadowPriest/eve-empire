// mkimage собирает OCI-образ в формате `docker save` без установленного Docker.
//
// Образ однослойный: статический бинарник + корневые сертификаты + пустые
// каталоги /data и /tmp. Тар кладётся на RB5009 и импортируется RouterOS:
//
//	/container/add file=eve-empire-arm64.tar interface=veth1 root-dir=disk1/eve
//
// Формат — «legacy» раскладка docker save (каталог слоя + manifest.json +
// repositories): её понимают и Docker, и контейнерный движок RouterOS.
package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strings"
	"time"
)

// Фиксированная дата: сборка одного и того же бинарника даёт побайтово
// одинаковый тар, иначе меняются все digest'ы.
var epoch = time.Unix(0, 0).UTC()

type imageConfig struct {
	Architecture string      `json:"architecture"`
	OS           string      `json:"os"`
	Created      string      `json:"created"`
	Config       runConfig   `json:"config"`
	RootFS       rootFS      `json:"rootfs"`
	History      []histEntry `json:"history"`
}

type runConfig struct {
	Env          []string            `json:"Env"`
	Entrypoint   []string            `json:"Entrypoint"`
	WorkingDir   string              `json:"WorkingDir"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts"`
}

type rootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

type histEntry struct {
	Created   string `json:"created"`
	CreatedBy string `json:"created_by"`
}

type manifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

func main() {
	bin := flag.String("bin", "dist/eve-empire", "linux/arm64 binary to package")
	certs := flag.String("certs", "dist/ca-certificates.crt", "PEM bundle for /etc/ssl/certs")
	out := flag.String("out", "dist/eve-empire-arm64.tar", "output image tarball")
	tag := flag.String("tag", "eve-empire:arm64", "image name:tag")
	arch := flag.String("arch", "arm64", "image architecture")
	workdir := flag.String("workdir", "/data", "container working directory")
	port := flag.String("port", "8080", "exposed tcp port")
	flag.Parse()

	binData, err := os.ReadFile(*bin)
	if err != nil {
		log.Fatalf("бинарник: %v", err)
	}
	certData, err := os.ReadFile(*certs)
	if err != nil {
		log.Fatalf("сертификаты: %v", err)
	}

	layer, err := buildLayer(binData, certData)
	if err != nil {
		log.Fatalf("слой: %v", err)
	}
	diffID := sha256.Sum256(layer)
	layerID := hex.EncodeToString(diffID[:])

	cfg := imageConfig{
		Architecture: *arch,
		OS:           "linux",
		Created:      epoch.Format(time.RFC3339),
		Config: runConfig{
			Env:          []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
			Entrypoint:   []string{"/app/eve-empire"},
			WorkingDir:   *workdir,
			ExposedPorts: map[string]struct{}{*port + "/tcp": {}},
		},
		RootFS:  rootFS{Type: "layers", DiffIDs: []string{"sha256:" + layerID}},
		History: []histEntry{{Created: epoch.Format(time.RFC3339), CreatedBy: "mkimage"}},
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	cfgSum := sha256.Sum256(cfgJSON)
	cfgName := hex.EncodeToString(cfgSum[:]) + ".json"

	name, ref, ok := strings.Cut(*tag, ":")
	if !ok {
		ref = "latest"
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatalf("вывод: %v", err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)

	// Порядок как у docker save: каталог слоя, конфиг, manifest.json, repositories.
	if err := writeDir(tw, layerID+"/", 0o755); err != nil {
		log.Fatal(err)
	}
	if err := writeFile(tw, layerID+"/VERSION", []byte("1.0"), 0o644); err != nil {
		log.Fatal(err)
	}
	legacy, _ := json.Marshal(map[string]any{
		"id":      layerID,
		"created": epoch.Format(time.RFC3339),
		"config":  cfg.Config,
	})
	if err := writeFile(tw, layerID+"/json", legacy, 0o644); err != nil {
		log.Fatal(err)
	}
	if err := writeFile(tw, layerID+"/layer.tar", layer, 0o644); err != nil {
		log.Fatal(err)
	}
	if err := writeFile(tw, cfgName, cfgJSON, 0o644); err != nil {
		log.Fatal(err)
	}

	manifest, _ := json.Marshal([]manifestEntry{{
		Config:   cfgName,
		RepoTags: []string{name + ":" + ref},
		Layers:   []string{layerID + "/layer.tar"},
	}})
	if err := writeFile(tw, "manifest.json", manifest, 0o644); err != nil {
		log.Fatal(err)
	}
	repos, _ := json.Marshal(map[string]map[string]string{name: {ref: layerID}})
	if err := writeFile(tw, "repositories", repos, 0o644); err != nil {
		log.Fatal(err)
	}

	if err := tw.Close(); err != nil {
		log.Fatal(err)
	}

	st, _ := f.Stat()
	fmt.Printf("%s\n  образ:  %s:%s (%s)\n  слой:   sha256:%s\n  размер: %.1f МБ\n",
		*out, name, ref, *arch, layerID[:12], float64(st.Size())/(1<<20))
}

// buildLayer собирает rootfs слоя: бинарник, сертификаты, рабочие каталоги.
func buildLayer(bin, certs []byte) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	dirs := []struct {
		name string
		mode int64
	}{
		{"app/", 0o755},
		{"data/", 0o755},
		{"etc/", 0o755},
		{"etc/ssl/", 0o755},
		{"etc/ssl/certs/", 0o755},
		{"tmp/", 0o1777},
	}
	for _, d := range dirs {
		if err := writeDir(tw, d.name, d.mode); err != nil {
			return nil, err
		}
	}
	if err := writeFile(tw, "app/eve-empire", bin, 0o755); err != nil {
		return nil, err
	}
	if err := writeFile(tw, "etc/ssl/certs/ca-certificates.crt", certs, 0o644); err != nil {
		return nil, err
	}
	// Резолвер Go читает /etc/resolv.conf; RouterOS подставит свой при dns= в
	// настройках контейнера, но без файла вовсе резолв уходит в 127.0.0.1.
	if err := writeFile(tw, "etc/resolv.conf", []byte("nameserver 8.8.8.8\nnameserver 1.1.1.1\n"), 0o644); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeDir(tw *tar.Writer, name string, mode int64) error {
	return tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir,
		Name:     path.Clean(name) + "/",
		Mode:     mode,
		ModTime:  epoch,
		Format:   tar.FormatGNU,
	})
}

func writeFile(tw *tar.Writer, name string, data []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     mode,
		Size:     int64(len(data)),
		ModTime:  epoch,
		Format:   tar.FormatGNU,
	}); err != nil {
		return err
	}
	_, err := io.Copy(tw, bytes.NewReader(data))
	return err
}
