// Command package builds the plugin for each supported platform and writes
// the files Admin > Plugins > "Install from a file" accepts into dist/:
//
//	dist/silo-pushover-notifications_<version>_<os>_<arch>.zip    manifest.json + plugin
//	dist/binaries/silo-pushover-notifications_<version>_<os>_<arch>   the bare binary
//	dist/SHA256SUMS.txt
//
// Run from the repository root: go run ./scripts/package
//
// A zip must hold exactly "manifest.json" and "plugin" at its root, and the
// manifest's "checksum" must be the SHA-256 of "plugin", so there is one zip
// per platform. A bare binary needs no checksum: Silo runs it with the
// "manifest" argument and the plugin reports its own. The zip is the safer
// choice because Silo never has to execute it to install it.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
)

func main() {
	out := flag.String("out", "dist", "output folder")
	targets := flag.String("targets", "linux/amd64,linux/arm64,darwin/arm64", "comma-separated os/arch list")
	manifestPath := flag.String("manifest", "manifest.json", "manifest to package")
	pkg := flag.String("pkg", ".", "Go package to build")
	verify := flag.String("verify", "", "only check this zip (os_arch taken from its name) and exit")
	flag.Parse()
	if *verify != "" {
		base := strings.TrimSuffix(filepath.Base(*verify), ".zip")
		parts := strings.Split(base, "_")
		if len(parts) < 4 {
			fmt.Fprintln(os.Stderr, "package: name must be <name>_<version>_<os>_<arch>.zip")
			os.Exit(1)
		}
		n := len(parts)
		if err := verifyZip(*verify, "silo.pushover", parts[n-3], parts[n-2], parts[n-1]); err != nil {
			fmt.Fprintln(os.Stderr, "package:", err)
			os.Exit(1)
		}
		fmt.Println("ok", *verify)
		return
	}
	if err := run(*out, *targets, *manifestPath, *pkg); err != nil {
		fmt.Fprintln(os.Stderr, "package:", err)
		os.Exit(1)
	}
}

func run(out, targets, manifestPath, pkg string) error {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var head struct {
		PluginID string `json:"plugin_id"`
		Version  string `json:"version"`
	}
	if err := json.Unmarshal(raw, &head); err != nil || head.Version == "" {
		return fmt.Errorf("read version from %s: %v", manifestPath, err)
	}
	binDir := filepath.Join(out, "binaries")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	var sums []string
	for _, t := range strings.Split(targets, ",") {
		goos, goarch, ok := strings.Cut(strings.TrimSpace(t), "/")
		if !ok {
			return fmt.Errorf("bad target %q", t)
		}
		name := fmt.Sprintf("silo-pushover-notifications_%s_%s_%s", head.Version, goos, goarch)
		binPath := filepath.Join(binDir, name)
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", binPath, pkg)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("build %s/%s: %w", goos, goarch, err)
		}
		bin, err := os.ReadFile(binPath)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(bin)
		checksum := hex.EncodeToString(sum[:])
		zipPath := filepath.Join(out, name+".zip")
		if err := writeZip(zipPath, raw, bin, checksum); err != nil {
			return err
		}
		if err := verifyZip(zipPath, head.PluginID, head.Version, goos, goarch); err != nil {
			return fmt.Errorf("verify %s: %w", zipPath, err)
		}
		zb, _ := os.ReadFile(zipPath)
		zsum := sha256.Sum256(zb)
		sums = append(sums, fmt.Sprintf("%s  %s", hex.EncodeToString(zsum[:]), filepath.Base(zipPath)),
			fmt.Sprintf("%s  binaries/%s", checksum, name))
		fmt.Printf("%s  (%d bytes, plugin sha256 %s)\n", zipPath, len(zb), checksum)
	}
	sort.Strings(sums)
	return os.WriteFile(filepath.Join(out, "SHA256SUMS.txt"), []byte(strings.Join(sums, "\n")+"\n"), 0o644)
}

// writeZip stores the manifest with its checksum filled in, then the binary.
func writeZip(path string, manifest, bin []byte, checksum string) error {
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(manifest))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return err
	}
	cs, _ := json.Marshal(checksum)
	m["checksum"] = cs
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	for _, e := range []struct {
		name string
		mode os.FileMode
		data []byte
	}{{"manifest.json", 0o644, body}, {"plugin", 0o755, bin}} {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		h.SetMode(e.mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err := w.Write(e.data); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return f.Close()
}

// verifyZip repeats the checks Silo's installer makes: both files at the
// root, a manifest the SDK accepts, the checksum matching the binary, and the
// platform listed.
func verifyZip(path, pluginID, version, goos, goarch string) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer zr.Close()
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return err
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		files[f.Name] = b
	}
	mb, pb := files["manifest.json"], files["plugin"]
	if mb == nil || pb == nil || len(files) != 2 {
		return fmt.Errorf("archive must hold exactly manifest.json and plugin")
	}
	m, err := publicmanifest.Load(mb)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(pb)
	if m.GetChecksum() != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("checksum does not match the binary")
	}
	if m.GetPluginId() != pluginID || m.GetVersion() != version || m.GetSiloApiVersion() != "v1" {
		return fmt.Errorf("unexpected identity %s %s api %s", m.GetPluginId(), m.GetVersion(), m.GetSiloApiVersion())
	}
	for _, p := range m.GetSupportedPlatforms() {
		if p.GetOs() == goos && p.GetArch() == goarch {
			return nil
		}
	}
	return fmt.Errorf("%s/%s is not in supported_platforms", goos, goarch)
}
