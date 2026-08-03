// Package config resolves runtime configuration: flags, data directory layout.
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	// Listen is the HTTP bind address. Defaults to loopback-only; binding to a
	// non-loopback address requires an explicit flag because this server holds
	// Proxmox root-equivalent API credentials.
	Listen string

	// DataDir holds the SQLite database, job logs, downloaded binaries, and the
	// secret-sealing key. Everything the app needs to survive a restart lives here.
	DataDir string

	// DBPath is DataDir/pvekube.db
	DBPath string
	// LogDir is DataDir/logs (per-job streamed command output)
	LogDir string
	// BinDir is DataDir/bin (downloaded kind/clusterctl/kubectl, SHA-verified)
	BinDir string
	// KubeconfigDir is DataDir/kubeconfigs
	KubeconfigDir string
}

func Load() (*Config, error) {
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address. Binding non-loopback exposes Proxmox credentials to your network — only do this on a trusted LAN.")
	dataDir := flag.String("data-dir", defaultDataDir(), "Directory for the database, logs, and downloaded tool binaries.")
	flag.Parse()

	c := &Config{
		Listen:  *listen,
		DataDir: *dataDir,
	}
	c.DBPath = filepath.Join(c.DataDir, "pvekube.db")
	c.LogDir = filepath.Join(c.DataDir, "logs")
	c.BinDir = filepath.Join(c.DataDir, "bin")
	c.KubeconfigDir = filepath.Join(c.DataDir, "kubeconfigs")

	for _, d := range []string{c.DataDir, c.LogDir, c.BinDir, c.KubeconfigDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("creating %s: %w", d, err)
		}
	}
	return c, nil
}

func defaultDataDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".pvekube")
	}
	return ".pvekube"
}
