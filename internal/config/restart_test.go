package config

import "testing"

func TestRequiresRestartTreatsRelativeSymlinksAsHot(t *testing.T) {
	current := &Config{}
	updated := &Config{RelativeSymlinks: true}
	if current.RequiresRestart(updated) {
		t.Fatal("relative symlink setting triggered a restart")
	}
}

func TestRequiresRestartIgnoresInactiveMountSettings(t *testing.T) {
	tests := []struct {
		name    string
		current Mount
		updated Mount
	}{
		{
			name:    "no mount",
			current: Mount{Type: MountTypeNone},
			updated: Mount{
				Type:      MountTypeNone,
				MountPath: "/unused",
				DFS:       DFS{CacheDir: "/unused/dfs"},
				Rclone:    Rclone{CacheDir: "/unused/rclone"},
				ExternalRclone: ExternalRclone{
					RCUrl: "http://127.0.0.1:9",
				},
			},
		},
		{
			name:    "legacy empty mount becomes no mount",
			current: Mount{},
			updated: Mount{
				Type:      MountTypeNone,
				MountPath: "/unused",
				DFS:       DFS{CacheDir: "/unused/dfs"},
			},
		},
		{
			name: "dfs ignores rclone settings",
			current: Mount{
				Type:      MountTypeDFS,
				MountPath: "/mnt/decypharr",
				DFS:       DFS{CacheDir: "/cache/dfs"},
			},
			updated: Mount{
				Type:      MountTypeDFS,
				MountPath: "/mnt/decypharr",
				DFS:       DFS{CacheDir: "/cache/dfs"},
				Rclone:    Rclone{CacheDir: "/unused/rclone"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := &Config{Mount: tt.current}
			updated := &Config{Mount: tt.updated}
			if current.RequiresRestart(updated) {
				t.Fatal("inactive mount settings triggered a restart")
			}
		})
	}
}

func TestRequiresRestartDetectsActiveMountChanges(t *testing.T) {
	current := &Config{Mount: Mount{
		Type:      MountTypeDFS,
		MountPath: "/mnt/decypharr",
		DFS:       DFS{CacheDir: "/cache/one"},
	}}
	updated := &Config{Mount: Mount{
		Type:      MountTypeDFS,
		MountPath: "/mnt/decypharr",
		DFS:       DFS{CacheDir: "/cache/two"},
	}}

	if !current.RequiresRestart(updated) {
		t.Fatal("active mount change did not trigger a restart")
	}
}
