package config

import (
	"testing"
)

func TestLoadFromEnvDefaults(t *testing.T) {
	cfg := LoadFromEnv()
	if cfg.MySQLHost != "127.0.0.1" {
		t.Errorf("expected default MySQLHost 127.0.0.1, got %s", cfg.MySQLHost)
	}
	if cfg.MySQLPort != 3306 {
		t.Errorf("expected default MySQLPort 3306, got %d", cfg.MySQLPort)
	}
	if cfg.ListenAddr != ":8090" {
		t.Errorf("expected default ListenAddr :8090, got %s", cfg.ListenAddr)
	}
	if cfg.LeaseDuration != 7200 {
		t.Errorf("expected default LeaseDuration 7200, got %d", cfg.LeaseDuration)
	}
	if cfg.RetryWait != 300 {
		t.Errorf("expected default RetryWait 300, got %d", cfg.RetryWait)
	}
	if cfg.MaxWorkers != 4 {
		t.Errorf("expected default MaxWorkers 4, got %d", cfg.MaxWorkers)
	}
}

func TestLoadFromEnvCustom(t *testing.T) {
	t.Setenv("MYSQL_HOST", "dbhost")
	t.Setenv("MYSQL_PORT", "3307")
	t.Setenv("LISTEN_ADDR", ":9090")
	t.Setenv("MAX_WORKERS", "8")

	cfg := LoadFromEnv()
	if cfg.MySQLHost != "dbhost" {
		t.Errorf("expected MySQLHost dbhost, got %s", cfg.MySQLHost)
	}
	if cfg.MySQLPort != 3307 {
		t.Errorf("expected MySQLPort 3307, got %d", cfg.MySQLPort)
	}
	if cfg.ListenAddr != ":9090" {
		t.Errorf("expected ListenAddr :9090, got %s", cfg.ListenAddr)
	}
	if cfg.MaxWorkers != 8 {
		t.Errorf("expected MaxWorkers 8, got %d", cfg.MaxWorkers)
	}
}

func TestDSN(t *testing.T) {
	cfg := &Config{
		MySQLUser: "user",
		MySQLPass: "pass",
		MySQLHost: "host",
		MySQLPort: 3306,
		Namespace: "phorge",
	}
	dsn := cfg.DSN()
	expected := "user:pass@tcp(host:3306)/phorge_worker?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s"
	if dsn != expected {
		t.Errorf("expected DSN %q, got %q", expected, dsn)
	}
}
