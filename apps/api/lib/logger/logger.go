package logger

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"livecart/apps/api/lib/config"
)

func New() (*zap.Logger, error) {
	env := config.Environment()

	var cfg zap.Config
	// staging usa o mesmo formato estruturado de producao (agregadores de log)
	if env == config.EnvProduction || env == config.EnvStaging {
		cfg = zap.NewProductionConfig()
		cfg.EncoderConfig.TimeKey = "ts"
		cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
		// O stacktrace automatico do zap em nivel Error aponta pro call site do
		// proprio log (sempre httpx/response.go), nunca pra origem do erro — no
		// agregador ele so enterra o campo "error", que e o que importa.
		cfg.DisableStacktrace = true
	} else {
		cfg = zap.NewDevelopmentConfig()
		cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		// Mesmo motivo de prod: o stack do call site não diz nada; panics têm
		// stack próprio no recover e erros carregam a cadeia de wraps.
		cfg.DisableStacktrace = true
	}

	// LOG_LEVEL sobrepoe o default do ambiente (ex.: debug em staging durante
	// uma investigacao, sem redeploy de codigo).
	if lvl := config.LogLevel.String(); lvl != "" {
		if parsed, err := zapcore.ParseLevel(lvl); err == nil {
			cfg.Level = zap.NewAtomicLevelAt(parsed)
		}
	}

	log, err := cfg.Build()
	if err != nil {
		return nil, err
	}

	// Todo log carrega o ambiente — separa staging de prod quando os streams
	// se misturam no agregador.
	return log.With(zap.String("env", env)), nil
}
