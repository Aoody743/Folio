package plugins

import (
	"slices"
	"testing"
)

func TestDistributionPHPDefaultMatchesNativeABI(t *testing.T) {
	for _, target := range []struct {
		distro, release, minor, repository string
	}{
		{"debian", "12", "8.2", "https://deb.debian.org/debian/"},
		{"debian", "13", "8.4", "https://deb.debian.org/debian/"},
		{"ubuntu", "24.04", "8.3", "https://archive.ubuntu.com/ubuntu/"},
	} {
		t.Run(target.distro+target.release, func(t *testing.T) {
			file, err := catalogFiles.Open("catalog/core.json")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			catalog, err := DecodeCatalog(file)
			if err != nil {
				t.Fatal(err)
			}
			catalog.useDistributionPHPDefault(Platform{OS: "linux", Arch: "amd64", Distro: target.distro, Release: target.release})
			manifest, _ := catalog.Get("php-apt")
			version, err := manifest.SelectVersion("")
			if err != nil || version.Version != target.minor {
				t.Fatalf("default PHP = %q, want %s; error: %v", version.Version, target.minor, err)
			}
			if !slices.Contains(version.Source.Packages, "php"+target.minor+"-fpm") ||
				!slices.Contains(version.Source.Packages, "php"+target.minor+"-cli") ||
				version.Source.Repository != target.repository || version.Source.RepositoryKeyURL != "" {
				t.Fatalf("default must use the distribution's signed FPM/CLI packages: %+v", version.Source)
			}
			defaults := 0
			for _, option := range manifest.Versions {
				if option.Default {
					defaults++
				}
			}
			if defaults != 1 {
				t.Fatalf("got %d default versions", defaults)
			}
			explicit, err := manifest.SelectVersion("8.4")
			if err != nil || explicit.Version != "8.4" {
				t.Fatalf("explicit PHP choice changed: %+v, %v", explicit, err)
			}
		})
	}
}
