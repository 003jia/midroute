package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config 是 Midroute 服务配置。
// 默认本地模式：仅监听 loopback；本地免登录；远程访问必须配置管理员密钥。
type Config struct {
	// HTTPAddr 监听地址，默认 127.0.0.1:18100。
	HTTPAddr string
	// DataDir 数据目录，默认 ./data。
	DataDir string
	// DBPath SQLite 数据库路径，默认 <DataDir>/midroute.db。
	DBPath string
	// AdminKey 管理员密钥。本地模式可为空（loopback 免登录）；远程监听时必须设置。
	AdminKey string
	// LocalOnly 默认 true，仅允许 loopback 访问。
	LocalOnly bool
	// StaticDir 管理前端静态目录（M6 起使用）。
	StaticDir string
	// LogLevel 结构化日志级别。
	LogLevel string
}

// Default 返回默认配置。
func Default() Config {
	return Config{
		HTTPAddr:  "127.0.0.1:18100",
		DataDir:   "data",
		LogLevel:  "info",
		LocalOnly: true,
	}
}

// Load 从环境变量与可选 JSON 文件加载配置，环境变量优先。
func Load() (Config, error) {
	cfg := Default()
	if p := os.Getenv("MIDROUTE_CONFIG"); p != "" {
		if err := loadFile(p, &cfg); err != nil {
			return cfg, err
		}
	}
	if v := os.Getenv("MIDROUTE_HTTP_ADDR"); v != "" {
		cfg.HTTPAddr = v
	}
	if v := os.Getenv("MIDROUTE_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("MIDROUTE_DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("MIDROUTE_ADMIN_KEY"); v != "" {
		cfg.AdminKey = v
	}
	if v := os.Getenv("MIDROUTE_LOCAL_ONLY"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("MIDROUTE_LOCAL_ONLY: %w", err)
		}
		cfg.LocalOnly = b
	}
	if v := os.Getenv("MIDROUTE_STATIC_DIR"); v != "" {
		cfg.StaticDir = v
	}
	if v := os.Getenv("MIDROUTE_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(cfg.DataDir, "midroute.db")
	}
	// 安全基线：远程监听必须有管理员密钥
	if !cfg.LocalOnly && cfg.AdminKey == "" {
		return cfg, fmt.Errorf("远程监听必须配置 MIDROUTE_ADMIN_KEY")
	}
	return cfg, nil
}

func loadFile(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var raw struct {
		HTTPAddr  *string `json:"httpAddr"`
		DataDir   *string `json:"dataDir"`
		DBPath    *string `json:"dbPath"`
		AdminKey  *string `json:"adminKey"`
		LocalOnly *bool   `json:"localOnly"`
		StaticDir *string `json:"staticDir"`
		LogLevel  *string `json:"logLevel"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	setStr := func(dst *string, v *string) {
		if v != nil && *v != "" {
			*dst = *v
		}
	}
	setStr(&cfg.HTTPAddr, raw.HTTPAddr)
	setStr(&cfg.DataDir, raw.DataDir)
	setStr(&cfg.DBPath, raw.DBPath)
	setStr(&cfg.AdminKey, raw.AdminKey)
	setStr(&cfg.StaticDir, raw.StaticDir)
	setStr(&cfg.LogLevel, raw.LogLevel)
	if raw.LocalOnly != nil {
		cfg.LocalOnly = *raw.LocalOnly
	}
	return nil
}

// String 返回脱敏后的配置摘要（不包含密钥）。
func (c Config) String() string {
	var sb strings.Builder
	sb.WriteString("http=" + c.HTTPAddr)
	sb.WriteString(" dataDir=" + c.DataDir)
	sb.WriteString(" localOnly=" + strconv.FormatBool(c.LocalOnly))
	if c.AdminKey != "" {
		sb.WriteString(" adminKey=<set>")
	} else {
		sb.WriteString(" adminKey=<unset>")
	}
	return sb.String()
}
