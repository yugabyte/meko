package cmd

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"cli_app/internal/apiclient"
	"cli_app/internal/config"
)

var (
	loginEmail     string
	loginPassword  string
	loginAPIServer string
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate with the meko API server",
	Long: `Log in to the meko API server using your email and password.
Tokens are stored locally so subsequent commands can authenticate automatically.`,
	RunE: runLogin,
}

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Remove stored credentials",
	Long:  `Log out by deleting locally stored authentication tokens.`,
	RunE:  runLogout,
}

func runLogin(_ *cobra.Command, _ []string) error {
	email, err := resolveEmail()
	if err != nil {
		return err
	}
	password, err := resolvePassword()
	if err != nil {
		return err
	}

	slog.Info("attempting login", "email", email, "server", loginAPIServer)

	client := apiclient.NewFromBaseURL(loginAPIServer)
	resp, err := client.Login(email, password)
	if err != nil {
		slog.Error("login failed", "email", email, "error", err)
		return fmt.Errorf("login failed: %w", err)
	}

	creds := &config.Credentials{
		AccessToken:  resp.AccessToken,
		IDToken:      resp.IDToken,
		RefreshToken: resp.RefreshToken,
		ExpiresIn:    resp.ExpiresIn,
		Email:        email,
		APIBaseURL:   loginAPIServer,
		LoggedInAt:   time.Now().UTC(),
	}
	if err := config.SaveCredentials(creds); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}

	slog.Info("login succeeded", "email", email)
	fmt.Printf("Logged in as %s\n", email)
	return nil
}

func runLogout(_ *cobra.Command, _ []string) error {
	if err := config.DeleteCredentials(); err != nil {
		return fmt.Errorf("logout failed: %w", err)
	}
	fmt.Println("Logged out successfully.")
	return nil
}

func resolveEmail() (string, error) {
	if loginEmail != "" {
		return loginEmail, nil
	}
	fmt.Print("Email: ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read email: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("email is required")
	}
	return line, nil
}

func resolvePassword() (string, error) {
	if loginPassword != "" {
		return loginPassword, nil
	}
	fmt.Print("Password: ")
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	pw := strings.TrimSpace(string(raw))
	if pw == "" {
		return "", fmt.Errorf("password is required")
	}
	return pw, nil
}

func init() {
	rootCmd.AddCommand(loginCmd)
	rootCmd.AddCommand(logoutCmd)

	loginCmd.Flags().StringVar(
		&loginEmail, "email", "", "account email")
	loginCmd.Flags().StringVar(
		&loginPassword, "password", "", "account password")
	loginCmd.Flags().StringVar(
		&loginAPIServer, "api-server", "https://mekodev.com",
		"API server base URL")
}
