// Copyright © The Sage Group plc or its licensors.

package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

type contextKey string

const contextKeyLogger contextKey = "logger"

type RootCommandOptions struct {
	logLevel  string
	logFormat string

	VersionCommandOptions
	ExpandCommandOptions
	ToArrayCommandOptions
}

func parseLogLevel(level string) (slog.Level, error) {
	var result slog.Level

	switch level {
	case "debug":
		result = slog.LevelDebug
	case "info":
		result = slog.LevelInfo
	case "warn":
		result = slog.LevelWarn
	case "error":
		result = slog.LevelError
	default:
		return result, fmt.Errorf("unable to parse error level: %s", level)
	}
	return result, nil
}

func getContextAndLogger(cmd *cobra.Command) (context.Context, *slog.Logger) {
	ctx := cmd.Context()
	if ctx == nil {
		panic("Must pass context into the command.")
	}
	logger, ok := ctx.Value(contextKeyLogger).(*slog.Logger)
	if !ok || logger == nil {
		panic("No logger passed in context")
	}
	return ctx, logger
}

func NewRootCommand(options *RootCommandOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   os.Args[0],
		Short: "Expands HelmRelease objects into generated templates",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				cmd.SilenceUsage = true
				return fmt.Errorf("must pass context into command")
			}
			logLevel, err := parseLogLevel(options.logLevel)
			if err != nil {
				err = fmt.Errorf(
					"unable to parse --log-level value %s (must be one of: debug, info, warn, error)",
					options.logLevel,
				)
				return err
			}
			writer := os.Stderr
			logOptions := &slog.HandlerOptions{AddSource: true, Level: logLevel}
			var handler slog.Handler

			switch options.logFormat {
			case "text":
				handler = slog.NewTextHandler(writer, logOptions)
			case "json":
				handler = slog.NewJSONHandler(writer, logOptions)
			default:
				return fmt.Errorf(
					"invalid --log-format value %s (valid values are text or json)",
					options.logFormat,
				)
			}
			logger := slog.New(handler)
			cmd.SetContext(context.WithValue(ctx, contextKeyLogger, logger))
			logger.Debug("Finished initialization")
			return nil
		},
	}
	command.PersistentFlags().StringVarP(
		&options.logLevel,
		"log-level",
		"",
		"warn",
		"Log level (debug, info, warn, error)",
	)
	command.PersistentFlags().StringVarP(
		&options.logFormat,
		"log-format",
		"",
		"text",
		"Log format (text or json)",
	)
	command.AddCommand(NewVersionCommand(&options.VersionCommandOptions))
	command.AddCommand(NewExpandCommand(&options.ExpandCommandOptions))
	command.AddCommand(NewToArrayCommand(&options.ToArrayCommandOptions))

	return command
}
