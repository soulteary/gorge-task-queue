package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	QueueBackend string // "mysql" (default) or "redis"

	MySQLHost string
	MySQLPort int
	MySQLUser string
	MySQLPass string
	Namespace string

	RedisAddr      string
	RedisPassword  string
	RedisDB        int
	RedisKeyPrefix string

	ListenAddr    string
	ServiceToken  string
	LeaseDuration int // seconds, default 7200 (2 hours)
	RetryWait     int // seconds, default 300 (5 minutes)
	PollInterval  int // milliseconds, default 1000
	MaxWorkers    int // concurrent lease processors, default 4
}

func LoadFromEnv() *Config {
	return &Config{
		QueueBackend: envStr("QUEUE_BACKEND", "mysql"),

		MySQLHost: envStr("MYSQL_HOST", "127.0.0.1"),
		MySQLPort: envInt("MYSQL_PORT", 3306),
		MySQLUser: envStr("MYSQL_USER", "phorge"),
		MySQLPass: envStr("MYSQL_PASS", ""),
		Namespace: envStr("STORAGE_NAMESPACE", "phorge"),

		RedisAddr:      envStr("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword:  envStr("REDIS_PASSWORD", ""),
		RedisDB:        envInt("REDIS_DB", 0),
		RedisKeyPrefix: envStr("REDIS_KEY_PREFIX", "gorge:tq:"),

		ListenAddr:    envStr("LISTEN_ADDR", ":8090"),
		ServiceToken:  envStr("SERVICE_TOKEN", ""),
		LeaseDuration: envInt("LEASE_DURATION", 7200),
		RetryWait:     envInt("RETRY_WAIT", 300),
		PollInterval:  envInt("POLL_INTERVAL", 1000),
		MaxWorkers:    envInt("MAX_WORKERS", 4),
	}
}

func (c *Config) DSN() string {
	return fmt.Sprintf(
		"%s:%s@tcp(%s:%d)/%s_worker?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s",
		c.MySQLUser, c.MySQLPass, c.MySQLHost, c.MySQLPort, c.Namespace,
	)
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return fallback
}
