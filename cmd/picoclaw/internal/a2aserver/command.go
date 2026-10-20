// PicoClaw - Ultra-lightweight personal AI agent

// Package a2aserver implements the `picoclaw a2a-server` command: the built-in
// private A2A messaging server (NOT the standard a2a-protocol.org service).
package a2aserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sipeed/picoclaw/cmd/picoclaw/internal"
	"github.com/sipeed/picoclaw/pkg/a2a/server"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

const defaultListen = "127.0.0.1:21220"

type loggerWriter struct{}

func (loggerWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if msg != "" {
		logger.InfoCF("a2a", msg, map[string]any{"component": "a2a-server"})
	}
	return len(p), nil
}

// NewA2AServerCommand builds the command tree:
//
//	picoclaw a2a-server            # run the server
//	picoclaw a2a-server init ...   # initialise a data directory
//	picoclaw a2a-server check ...  # read-only self check
func NewA2AServerCommand() *cobra.Command {
	var configPath string
	var listen, dataDir, domain, signMode string
	var retention time.Duration
	var allowDefaultAgents, force bool

	cmd := &cobra.Command{
		Use:   "a2a-server",
		Short: "Run the built-in private A2A messaging server",
		Long: `Run the built-in private A2A direct-message server.

This is the self-hosted private messaging platform (Bearer token + Ed25519
signatures, default 127.0.0.1:21220). It is NOT the standard a2a-protocol.org
service. The client side lives in channel_list.a2a.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The server process only needs the a2a_server section. Reading it
			// minimally keeps shadow configs (which may not carry a full
			// picoclaw config version) working and avoids touching anything
			// else in the config file.
			section := readA2AConfig(resolveConfigPath(configPath))
			if !section.Enabled && !force {
				return fmt.Errorf(
					"a2a server is disabled in config (a2a_server.enabled=false); " +
						"use --force to run anyway",
				)
			}
			if cmd.Flags().Changed("listen") {
				section.Listen = listen
			}
			if cmd.Flags().Changed("data-dir") {
				section.DataDir = dataDir
			}
			if cmd.Flags().Changed("domain") {
				section.Domain = domain
			}
			if cmd.Flags().Changed("sign-mode") {
				section.SignMode = signMode
			}
			if cmd.Flags().Changed("allow-default-agents") {
				section.AllowDefaultAgents = allowDefaultAgents
			}
			if section.Listen == "" {
				section.Listen = defaultListen
			}
			if section.Domain == "" {
				section.Domain = "localhost"
			}
			if strings.TrimSpace(section.DataDir) == "" {
				return errors.New("a2a_server.data_dir is required")
			}
			retentionValue := retention
			if !cmd.Flags().Changed("retention") {
				switch hours := section.RetentionHours; {
				case hours < 0:
					retentionValue = 0
				case hours == 0:
					retentionValue = 24 * time.Hour
				default:
					retentionValue = time.Duration(hours) * time.Hour
				}
			}

			logger.SetTimestampUTC()
			logger.SetLevelFromString(section.LogLevel)

			inst, err := server.New(server.Options{
				Listen:             section.Listen,
				DataDir:            expandHome(section.DataDir),
				Domain:             section.Domain,
				Retention:          retentionValue,
				SignMode:           section.SignMode,
				AllowDefaultAgents: section.AllowDefaultAgents,
				Logger:             log.New(loggerWriter{}, "", 0),
			})
			if err != nil {
				return err
			}
			if err := inst.Start(); err != nil {
				_ = inst.Stop(context.Background())
				return err
			}

			ctx, stopSignals := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stopSignals()
			<-ctx.Done()

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			logger.InfoCF("a2a", "shutting down private A2A server", map[string]any{"listen": inst.Addr()})
			return inst.Stop(shutdownCtx)
		},
	}
	cmd.PersistentFlags().StringVar(&configPath, "config", "", "config file path (default: $PICOCLAW_CONFIG or ~/.picoclaw/config.json)")
	cmd.Flags().StringVar(&listen, "listen", "", "listen address (loopback only; default 127.0.0.1:21220)")
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "data directory (default a2a_server.data_dir)")
	cmd.Flags().StringVar(&domain, "domain", "", "public domain for did:web")
	cmd.Flags().DurationVar(&retention, "retention", 0, "message retention (default a2a_server.retention_hours)")
	cmd.Flags().StringVar(&signMode, "sign-mode", "", "signature mode: off|warn|strict")
	cmd.Flags().BoolVar(&allowDefaultAgents, "allow-default-agents", false, "allow generating the legacy alice/bob/carol/dave allowlist (development only)")
	cmd.Flags().BoolVar(&force, "force", false, "run even when a2a_server.enabled=false")

	cmd.AddCommand(
		newInitCommand(&configPath),
		newCheckCommand(&configPath),
	)
	return cmd
}

func newInitCommand(configPath *string) *cobra.Command {
	var dataDir, domain, agents, addAgents string
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialise a data directory (allowlist, tokens, platform key)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			section := readA2AConfig(resolveConfigPath(*configPath))
			if !cmd.Flags().Changed("data-dir") {
				dataDir = section.DataDir
			}
			if !cmd.Flags().Changed("domain") {
				domain = section.Domain
			}
			if strings.TrimSpace(dataDir) == "" {
				return errors.New("data_dir is required (--data-dir or a2a_server.data_dir)")
			}
			agentList, err := splitList(agents)
			if err != nil {
				return err
			}
			addList, err := splitList(addAgents)
			if err != nil {
				return err
			}
			result, err := server.InitData(server.InitOptions{
				DataDir:   expandHome(dataDir),
				Domain:    domain,
				Agents:    agentList,
				AddAgents: addList,
			})
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{
					"success":  true,
					"data_dir": result.DataDir,
					"added":    result.Added,
					"tokens":   result.Tokens,
				})
			}
			fmt.Printf("A2A data initialised: %s\n", result.DataDir)
			for _, slug := range result.Added {
				fmt.Printf("  %s\ttoken\t%s\n", slug, result.Tokens[slug])
			}
			fmt.Println("Save these tokens now — they are shown only once (tokens.txt has a 0600 plaintext copy).")
			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "data directory (default a2a_server.data_dir)")
	cmd.Flags().StringVar(&domain, "domain", "", "public domain for did:web")
	cmd.Flags().StringVar(&agents, "agents", "", "comma-separated agent slugs to create")
	cmd.Flags().StringVar(&addAgents, "add-agents", "", "append agents to an existing allowlist and mint tokens")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print machine-readable JSON")
	return cmd
}

func newCheckCommand(configPath *string) *cobra.Command {
	var dataDir, listen, domain, signMode string
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "check",
		Short: "Read-only self check of the A2A data directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			section := readA2AConfig(resolveConfigPath(*configPath))
			if !cmd.Flags().Changed("data-dir") {
				dataDir = section.DataDir
			}
			if !cmd.Flags().Changed("listen") {
				listen = section.Listen
			}
			if !cmd.Flags().Changed("sign-mode") {
				signMode = section.SignMode
			}
			if listen == "" {
				listen = defaultListen
			}
			report := server.CheckData(server.CheckOptions{
				DataDir:  expandHome(dataDir),
				Listen:   listen,
				Domain:   domain,
				SignMode: signMode,
			})
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					return err
				}
			} else {
				printCheckReport(report)
			}
			if !report.OK {
				return fmt.Errorf("self-check failed with %d error(s)", len(report.Errors))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "data directory (default a2a_server.data_dir)")
	cmd.Flags().StringVar(&listen, "listen", "", "listen address to check (default a2a_server.listen)")
	cmd.Flags().StringVar(&domain, "domain", "", "public domain for did:web")
	cmd.Flags().StringVar(&signMode, "sign-mode", "", "signature mode to validate")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print machine-readable JSON")
	return cmd
}

func printCheckReport(rep *server.CheckReport) {
	fmt.Printf("data_dir:      %s\n", rep.DataDir)
	fmt.Printf("agents:        %d\n", rep.Agents)
	fmt.Printf("messages:      %d\n", rep.Messages)
	fmt.Printf("conversations: %d\n", rep.Conversations)
	fmt.Printf("sign_mode:     %s\n", rep.SignMode)
	listenLine := rep.Listen
	if rep.ListenInUse {
		listenLine += " (in use — server likely running)"
	}
	fmt.Printf("listen:        %s\n", listenLine)
	if rep.OK {
		fmt.Println("result:        OK")
	} else {
		fmt.Println("result:        FAILED")
	}
	for _, e := range rep.Errors {
		fmt.Printf("error:   %s\n", e)
	}
	for _, w := range rep.Warnings {
		fmt.Printf("warning: %s\n", w)
	}
}

func resolveConfigPath(flag string) string {
	if strings.TrimSpace(flag) != "" {
		return expandHome(flag)
	}
	return internal.GetConfigPath()
}

// readA2AConfig reads only the a2a_server section. Unlike config.LoadConfig it
// never writes or generates anything, so `check` stays strictly read-only.
func readA2AConfig(path string) config.A2AServerConfig {
	var wrapper struct {
		A2AServer config.A2AServerConfig `json:"a2a_server"`
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return wrapper.A2AServer
	}
	if err := json.Unmarshal(b, &wrapper); err != nil {
		return config.A2AServerConfig{}
	}
	return wrapper.A2AServer
}

func splitList(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errors.New("agent list contains an empty item")
		}
		out = append(out, part)
	}
	return out, nil
}

func expandHome(path string) string {
	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
