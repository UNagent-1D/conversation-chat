package config

import (
	"log"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	ServerPort    string
	AppVersion    string
	GinMode       string
	RedisURL      string
	MongoURI      string
	MongoDB       string
	ACRServiceURL string
	TenantServiceURL string
	AuthServiceURL   string
	OpenAIAPIKey     string
	OpenAIBaseURL    string
	DefaultIdleTimeoutSeconds int
	AuthStub       bool
	AuthStubClaims StubClaims
}

type StubClaims struct {
	UserID     string
	Role       string
	TenantID   string
	TenantSlug string
	Email      string
}

func Load() *Config {
	if err := godotenv.Load(); err != nil {
		log.Println("no .env file found, reading from environment")
	}

	return &Config{
		ServerPort:               getEnv("SERVER_PORT", "8082"),
		AppVersion:               getEnv("APP_VERSION", "1.0.0"),
		GinMode:                  getEnv("GIN_MODE", "debug"),
		RedisURL:                 getEnv("REDIS_URL", "redis://localhost:6379/0"),
		MongoURI:                 getEnv("MONGO_URI", "mongodb://localhost:27017"),
		MongoDB:                  getEnv("MONGO_DB", "conversatory"),
		ACRServiceURL:            getEnv("ACR_SERVICE_URL", "http://localhost:8081"),
		TenantServiceURL:         getEnv("TENANT_SERVICE_URL", "http://localhost:8080"),
		AuthServiceURL:           getEnv("AUTH_SERVICE_URL", "http://localhost:9090"),
		OpenAIAPIKey:             getEnv("OPENAI_API_KEY", ""),
		OpenAIBaseURL:            getEnv("OPENAI_BASE_URL", ""),
		DefaultIdleTimeoutSeconds: getInt("DEFAULT_IDLE_TIMEOUT_SECONDS", 300),
		AuthStub:                 getBool("AUTH_STUB", false),
		AuthStubClaims: StubClaims{
			UserID:     getEnv("AUTH_STUB_USER_ID", "00000000-0000-0000-0000-000000000001"),
			Role:       getEnv("AUTH_STUB_ROLE", "app_admin"),
			TenantID:   getEnv("AUTH_STUB_TENANT_ID", ""),
			TenantSlug: getEnv("AUTH_STUB_TENANT_SLUG", ""),
			Email:      getEnv("AUTH_STUB_EMAIL", "internal@platform.local"),
		},
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func getInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
