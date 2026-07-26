package config

import (
	"fmt"
	"net"
	"strings"
)

// ValidateDeployment catches network exposure that is syntactically valid but
// unsafe for a service intended to leave loopback. It is deliberately separate
// from Validate because Validate also drives the first-run setup state.
func (c *Config) ValidateDeployment() error {
	if IsLoopbackBindAddress(c.BindAddress) {
		return nil
	}
	if !c.UseAuth {
		return fmt.Errorf(
			"authentication is required on a non-loopback listener; use --set-auth",
		)
	}
	if !c.DisableWebDav && !c.EnableWebdavAuth {
		return fmt.Errorf(
			"WebDAV must be disabled or protected by authentication on a non-loopback listener",
		)
	}
	return nil
}

func IsLoopbackBindAddress(bindAddress string) bool {
	host := strings.TrimSpace(bindAddress)
	if strings.EqualFold(host, "localhost") {
		return true
	}

	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	if zoneIndex := strings.LastIndexByte(host, '%'); zoneIndex >= 0 {
		host = host[:zoneIndex]
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
