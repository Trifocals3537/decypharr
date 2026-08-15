package config

import (
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const (
	MinAuthPasswordBytes = 6
	MaxAuthPasswordBytes = 72
	MaxAuthUsernameBytes = 128
)

func VerifyAuth(username, password string) bool {
	if username == "" {
		return false
	}
	auth := Get().GetAuth()
	if auth == nil {
		return false
	}
	if username != auth.Username {
		return false
	}
	err := bcrypt.CompareHashAndPassword([]byte(auth.Password), []byte(password))
	return err == nil
}

// SetAuthCredentials validates and persists a username and bcrypt password
// hash while retaining the installation's existing session secret and API
// token. The caller must Save the main configuration after all related changes
// are ready so use_auth becomes durable.
func (c *Config) SetAuthCredentials(username, password string) error {
	username = strings.TrimSpace(username)
	if err := ValidateAuthCredentials(username, password); err != nil {
		return err
	}

	hashedPassword, err := bcrypt.GenerateFromPassword(
		[]byte(password),
		bcrypt.DefaultCost,
	)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	auth, err := c.loadAuth()
	if err != nil {
		return err
	}
	auth.Username = username
	auth.Password = string(hashedPassword)
	auth.SessionVersion++
	if auth.SessionVersion == 0 {
		auth.SessionVersion = 1
	}
	c.UseAuth = true
	if err := c.SaveAuth(auth); err != nil {
		return fmt.Errorf("save authentication: %w", err)
	}
	return nil
}

func ValidateAuthCredentials(username, password string) error {
	username = strings.TrimSpace(username)
	switch {
	case username == "":
		return fmt.Errorf("username is required")
	case len(username) > MaxAuthUsernameBytes:
		return fmt.Errorf("username must be at most %d bytes", MaxAuthUsernameBytes)
	case len(password) < MinAuthPasswordBytes:
		return fmt.Errorf(
			"password must be at least %d bytes",
			MinAuthPasswordBytes,
		)
	case len(password) > MaxAuthPasswordBytes:
		return fmt.Errorf(
			"password must be at most %d bytes",
			MaxAuthPasswordBytes,
		)
	default:
		return nil
	}
}
