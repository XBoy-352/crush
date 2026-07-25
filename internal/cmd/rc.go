package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/charmbracelet/crush/internal/remotecontrol"
	"github.com/spf13/cobra"
)

var (
	rcRelayURL string
	rcUser     string
	rcPass     string
)

func init() {
	rcCmd.Flags().StringVarP(&rcRelayURL, "relay", "r", "ws://localhost:8080", "Relay server address (ws:// or wss://)")
	rcCmd.Flags().StringVarP(&rcUser, "user", "u", "admin", "Authentication username")
	rcCmd.Flags().StringVarP(&rcPass, "pass", "p", "crushsecret", "Authentication password")

	rootCmd.AddCommand(rcCmd)
}

var rcCmd = &cobra.Command{
	Use:     "remote-control",
	Aliases: []string{"rc"},
	Short:   "Start Remote Control agent bridge to OCI Relay & Mobile App",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		sessionID := fmt.Sprintf("crush-cli-%d", time.Now().Unix())

		fmt.Printf("==================================================\n")
		fmt.Printf("  Crush Remote Control Bridge\n")
		fmt.Printf("  Target Relay: %s\n", rcRelayURL)
		fmt.Printf("  User: %s\n", rcUser)
		fmt.Printf("  Session ID: %s\n", sessionID)
		fmt.Printf("==================================================\n\n")

		client := remotecontrol.NewClient(remotecontrol.Config{
			RelayURL: rcRelayURL,
			Username: rcUser,
			Password: rcPass,
		})

		client.SetPromptHandler(func(prompt string) {
			fmt.Printf("\n[Remote Prompt Received]: %s\n", prompt)
			_ = client.SendStreamChunk("assistant", fmt.Sprintf("Received remote prompt: '%s'. Processing...", prompt))
		})

		client.SetCancelHandler(func() {
			fmt.Printf("\n[Remote Control] Received Task Cancel Request\n")
		})

		if err := client.Connect(ctx, sessionID); err != nil {
			return fmt.Errorf("remote control connection failed: %w", err)
		}
		defer client.Close()

		fmt.Printf("✓ Connected to Relay Server successfully!\n")
		fmt.Printf("  Access your Mobile PWA App at your OCI VM URL.\n")
		fmt.Printf("  Press Ctrl+C to stop remote control bridge.\n\n")

		<-ctx.Done()
		fmt.Printf("\nDisconnecting Remote Control bridge...\n")
		return nil
	},
}
