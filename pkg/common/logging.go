// Copyright (c) 2023-2025 AccelByte Inc. All Rights Reserved.
// This is licensed software from AccelByte Inc, for limitations
// and restrictions contact your company contract manager.

package common

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
)

// InterceptorLogger adapts slog logger to interceptor logger.
// This code is referenced from https://github.com/grpc-ecosystem/go-grpc-middleware/
func InterceptorLogger(logger *slog.Logger) logging.Logger {
	return logging.LoggerFunc(func(ctx context.Context, lvl logging.Level, msg string, fields ...any) {
		attrs := make([]slog.Attr, 0, len(fields)/2)
		iterator := logging.Fields(fields).Iterator()
		for iterator.Next() {
			fieldName, fieldValue := iterator.At()
			attrs = append(attrs, slog.Any(fieldName, fieldValue))
		}

		var slogLevel slog.Level
		switch lvl {
		case logging.LevelDebug:
			slogLevel = slog.LevelDebug
		case logging.LevelInfo:
			slogLevel = slog.LevelInfo
		case logging.LevelWarn:
			slogLevel = slog.LevelWarn
		case logging.LevelError:
			slogLevel = slog.LevelError
		default:
			panic(fmt.Sprintf("unknown level %v", lvl))
		}

		logger.LogAttrs(ctx, slogLevel, msg, attrs...)
	})
}
