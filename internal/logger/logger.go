package logger

import (
	"os"
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type Logger struct {
	*zap.Logger
	closer    *os.File
	closeOnce sync.Once
	closeErr  error
}

func New(level, output string) *Logger {
	var zapLevel zapcore.Level
	switch level {
	case "debug":
		zapLevel = zapcore.DebugLevel
	case "info":
		zapLevel = zapcore.InfoLevel
	case "warn":
		zapLevel = zapcore.WarnLevel
	case "error":
		zapLevel = zapcore.ErrorLevel
	default:
		zapLevel = zapcore.InfoLevel
	}

	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(zapLevel)
	config.EncoderConfig.TimeKey = "timestamp"
	config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	var writeSyncer zapcore.WriteSyncer
	var closer *os.File
	if output == "stdout" {
		writeSyncer = zapcore.AddSync(os.Stdout)
	} else {
		file, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			writeSyncer = zapcore.AddSync(os.Stdout)
		} else {
			writeSyncer = zapcore.AddSync(file)
			closer = file
		}
	}

	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(config.EncoderConfig),
		writeSyncer,
		zapLevel,
	)

	logger := zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel))

	return &Logger{Logger: logger, closer: closer}
}

// Close flushes the logger and releases an owned output file. It never closes
// stdout and is safe to call more than once.
func (l *Logger) Close() error {
	if l == nil || l.Logger == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		if l.closer == nil {
			return
		}
		if err := l.Logger.Sync(); err != nil {
			l.closeErr = err
		}
		if err := l.closer.Close(); err != nil && l.closeErr == nil {
			l.closeErr = err
		}
	})
	return l.closeErr
}

func (l *Logger) Fatal(msg string, fields ...interface{}) {
	zapFields := make([]zap.Field, 0, len(fields))
	for _, f := range fields {
		switch v := f.(type) {
		case error:
			zapFields = append(zapFields, zap.Error(v))
		default:
			zapFields = append(zapFields, zap.Any("field", v))
		}
	}
	l.Logger.Fatal(msg, zapFields...)
}
