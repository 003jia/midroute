package config

import (
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("MIDROUTE_HTTP_ADDR", "")
	t.Setenv("MIDROUTE_ADMIN_KEY", "")
	t.Setenv("MIDROUTE_LOCAL_ONLY", "")
	t.Setenv("MIDROUTE_DATA_DIR", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != "127.0.0.1:18100" {
		t.Fatalf("http=%q", cfg.HTTPAddr)
	}
	if !cfg.LocalOnly {
		t.Fatal("default must be localOnly")
	}
}

func TestLoadRemoteRequiresAdminKey(t *testing.T) {
	t.Setenv("MIDROUTE_LOCAL_ONLY", "false")
	t.Setenv("MIDROUTE_ADMIN_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("remote without admin key must fail")
	}
	t.Setenv("MIDROUTE_ADMIN_KEY", "test-admin-key")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LocalOnly {
		t.Fatal("localOnly should be false")
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("MIDROUTE_HTTP_ADDR", "0.0.0.0:9999")
	t.Setenv("MIDROUTE_DATA_DIR", "/tmp/midroute-test-data")
	t.Setenv("MIDROUTE_ADMIN_KEY", "k")
	t.Setenv("MIDROUTE_LOCAL_ONLY", "false")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != "0.0.0.0:9999" || cfg.DataDir != "/tmp/midroute-test-data" {
		t.Fatalf("override failed: %+v", cfg)
	}
	if cfg.DBPath != "/tmp/midroute-test-data/midroute.db" {
		t.Fatalf("dbpath=%q", cfg.DBPath)
	}
}

func TestLoadInvalidBool(t *testing.T) {
	t.Setenv("MIDROUTE_LOCAL_ONLY", "notabool")
	if _, err := Load(); err == nil {
		t.Fatal("invalid bool must fail")
	}
}

func TestStringRedactsAdminKey(t *testing.T) {
	cfg := Default()
	cfg.AdminKey = "super-secret-admin-key-123456789"
	s := cfg.String()
	if strings.Contains(s, "super-secret") {
		t.Fatalf("config string leaked admin key: %q", s)
	}
}
