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

func TestLoadFromEnvQueueBackendDefaults(t *testing.T) {
	cfg := LoadFromEnv()
	if cfg.QueueBackend != "mysql" {
		t.Errorf("expected default QueueBackend 'mysql', got %s", cfg.QueueBackend)
	}
	if cfg.RedisAddr != "127.0.0.1:6379" {
		t.Errorf("expected default RedisAddr '127.0.0.1:6379', got %s", cfg.RedisAddr)
	}
	if cfg.RedisPassword != "" {
		t.Errorf("expected default RedisPassword '', got %s", cfg.RedisPassword)
	}
	if cfg.RedisDB != 0 {
		t.Errorf("expected default RedisDB 0, got %d", cfg.RedisDB)
	}
	if cfg.RedisKeyPrefix != "gorge:tq:" {
		t.Errorf("expected default RedisKeyPrefix 'gorge:tq:', got %s", cfg.RedisKeyPrefix)
	}
}

func TestLoadFromEnvRedisCustom(t *testing.T) {
	t.Setenv("QUEUE_BACKEND", "redis")
	t.Setenv("REDIS_ADDR", "redis-host:6380")
	t.Setenv("REDIS_PASSWORD", "secret")
	t.Setenv("REDIS_DB", "3")
	t.Setenv("REDIS_KEY_PREFIX", "myapp:")

	cfg := LoadFromEnv()
	if cfg.QueueBackend != "redis" {
		t.Errorf("expected QueueBackend 'redis', got %s", cfg.QueueBackend)
	}
	if cfg.RedisAddr != "redis-host:6380" {
		t.Errorf("expected RedisAddr 'redis-host:6380', got %s", cfg.RedisAddr)
	}
	if cfg.RedisPassword != "secret" {
		t.Errorf("expected RedisPassword 'secret', got %s", cfg.RedisPassword)
	}
	if cfg.RedisDB != 3 {
		t.Errorf("expected RedisDB 3, got %d", cfg.RedisDB)
	}
	if cfg.RedisKeyPrefix != "myapp:" {
		t.Errorf("expected RedisKeyPrefix 'myapp:', got %s", cfg.RedisKeyPrefix)
	}
}
