package logger

import (
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func New() (*zap.Logger, error) {
	env := os.Getenv("APP_ENV")

	var config zap.Config
	if env == "production" {
		config = zap.NewProductionConfig()
		config.EncoderConfig.TimeKey = "ts"
		config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	} else {
		config = zap.NewDevelopmentConfig()
		config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}

	return config.Build(zap.AddCallerSkip(0))
}
