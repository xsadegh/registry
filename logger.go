package main

import (
	"io"

	"github.com/sirupsen/logrus"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func newFxLogger(log *zap.Logger) fxevent.Logger {
	l := &fxevent.ZapLogger{Logger: log.Named("fx")}
	l.UseLogLevel(zapcore.DebugLevel)
	return l
}

func newLogger(lc fx.Lifecycle, cfg Config) (*zap.Logger, error) {
	c := cfg.Log
	level, err := zap.ParseAtomicLevel(c.Level)
	if err != nil {
		return nil, err
	}
	zc := zap.NewProductionConfig()
	zc.Level = level
	zc.Encoding = c.Format
	zc.Sampling = nil
	zc.DisableStacktrace = true
	zc.EncoderConfig.TimeKey = "time"
	zc.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	if c.Format == "console" {
		zc.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}
	log, err := zc.Build()
	if err != nil {
		return nil, err
	}

	logrus.SetOutput(io.Discard)
	logrus.SetLevel(logrus.WarnLevel)
	if level.Level() == zapcore.DebugLevel {
		logrus.SetLevel(logrus.DebugLevel)
	}
	logrus.AddHook(logrusHook{log.Named("distribution").WithOptions(zap.WithCaller(false))})
	lc.Append(fx.StopHook(func() { _ = log.Sync() }))
	return log, nil
}

type logrusHook struct {
	log *zap.Logger
}

func (logrusHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

func (h logrusHook) Fire(e *logrus.Entry) error {
	fields := make([]zap.Field, 0, len(e.Data))
	for k, v := range e.Data {
		if k != "go.version" {
			fields = append(fields, zap.Any(k, v))
		}
	}
	level := zapcore.DebugLevel
	switch e.Level {
	case logrus.PanicLevel, logrus.FatalLevel, logrus.ErrorLevel:
		level = zapcore.ErrorLevel
	case logrus.WarnLevel:
		level = zapcore.WarnLevel
	case logrus.InfoLevel:
		level = zapcore.InfoLevel
	default:
	}
	if status, ok := e.Data["http.response.status"].(int); ok && status < 500 {
		level = zapcore.DebugLevel
	}
	h.log.Log(level, e.Message, fields...)
	return nil
}
