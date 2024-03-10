package logger

import (
	"go.uber.org/zap"
)

var (
	logger      *zap.Logger
	sugarLogger *zap.SugaredLogger
)

func init() {
	cfg := zap.NewProductionConfig()
	cfg.OutputPaths = []string{"stdout"} // Replace with your desired log file path
	logger, _ = cfg.Build()

	sugarLogger = logger.Sugar()
}

// Use the logger variable to log messages throughout your code
func Info(msg string, fields ...zap.Field) {
	logger.Info(msg, fields...)
}

func Infof(template string, args ...interface{}) {
	sugarLogger.Infof(template, args)
}

func Warn(msg string, fields ...zap.Field) {
	logger.Warn(msg, fields...)
}

func Warnf(template string, args ...interface{}) {
	sugarLogger.Warnf(template, args)
}

func Error(msg string, fields ...zap.Field) {
	logger.Error(msg, fields...)
}

func Errorf(template string, args ...interface{}) {
	sugarLogger.Errorf(template, args...)
}

func Debug(msg string, fields ...zap.Field) {
	logger.Debug(msg, fields...)
}

func Debugf(template string, args ...interface{}) {
	sugarLogger.Debugf(template, args...)
}

func Fatal(msg string, fields ...zap.Field) {
	logger.Fatal(msg, fields...)
}

func Fatalf(template string, args ...interface{}) {
	sugarLogger.Fatalf(template, args...)
}
