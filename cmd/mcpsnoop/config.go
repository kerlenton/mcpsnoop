package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
)

type config struct {
	Label         string
	TraceFile     string
	NoTrace       bool
	RedactSecrets bool
	RedactKeys    []string
}

func loadConfig() (config, bool, error) {
	const configFile = ".mcpsnoop.toml"

	f, err := os.Open(configFile)
	if os.IsNotExist(err) {
		return config{}, false, nil
	}
	if err != nil {
		return config{}, false, err
	}
	defer f.Close()

	cfg, err := parseConfig(f)
	if err != nil {
		return config{}, false, err
	}

	return cfg, true, nil
}

func parseValue(raw string) (string, error) {
	value := strings.TrimSpace(raw)

	if strings.HasPrefix(value, "[") {
		return "", fmt.Errorf("array values are not supported")
	}

	if strings.HasPrefix(value, `"`) {
		rest := value[1:]
		quote := strings.Index(rest, `"`)

		if quote == -1 {
			return "", fmt.Errorf("unterminated quoted value")
		}

		end := quote + 1

		if strings.TrimSpace(value[end+1:]) != "" {
			return "", fmt.Errorf("unexpected content after quoted value")
		}

		return value[1:end], nil
	}

	if i := strings.Index(value, "#"); i >= 0 {
		return "", fmt.Errorf("unexpected content after value")
	}

	return value, nil
}

func parseConfig(r io.Reader) (config, error) {
	var cfg config

	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return config{}, fmt.Errorf("invalid config line: %q", line)
		}

		key := strings.TrimSpace(parts[0])

		value, err := parseValue(parts[1])
		if err != nil {
			return config{}, fmt.Errorf("invalid value for %q: %w", key, err)
		}

		switch key {
		case "label":
			cfg.Label = value

		case "trace-file":
			cfg.TraceFile = value

		case "no-trace":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return config{}, fmt.Errorf("invalid value for %q: %w", key, err)
			}
			cfg.NoTrace = b

		case "redact-secrets":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return config{}, fmt.Errorf("invalid value for %q: %w", key, err)
			}
			cfg.RedactSecrets = b

		case "redact-key":
			var keys redactKeysFlag
			if err := keys.Set(value); err != nil {
				return config{}, err
			}
			cfg.RedactKeys = []string(keys)

		default:
			return config{}, fmt.Errorf("unknown config key %q", key)
		}
	}

	if err := scanner.Err(); err != nil {
		return config{}, err
	}

	return cfg, nil
}

func applyConfig(
	fs *pflag.FlagSet,
	cfg config,
	ok bool,
	label, traceFile *string,
	noTrace, redactSecrets *bool,
	redactKeys *redactKeysFlag,
) {
	if !ok {
		return
	}

	if !fs.Changed("label") {
		*label = cfg.Label
	}

	if traceFile != nil && !fs.Changed("trace-file") {
		*traceFile = cfg.TraceFile
	}

	if !fs.Changed("no-trace") {
		*noTrace = cfg.NoTrace
	}

	if !fs.Changed("redact-secrets") {
		*redactSecrets = cfg.RedactSecrets
	}

	if !fs.Changed("redact-key") {
		*redactKeys = redactKeysFlag(cfg.RedactKeys)
	}
}
