package plugins

import "testing"

func TestFirstReleaseDatabaseCatalogSupportsUbuntu2404AMD64(t *testing.T) {
	catalog, err := BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	ubuntu := Platform{OS: "linux", Arch: "amd64", Distro: "ubuntu", Release: "24.04"}
	unsupported := []Platform{
		{OS: "linux", Arch: "amd64", Distro: "ubuntu", Release: "22.04"},
		{OS: "linux", Arch: "arm64", Distro: "ubuntu", Release: "24.04"},
	}
	for _, id := range []string{"mysql", "mysql-community", "postgresql"} {
		manifest, err := catalog.Get(id)
		if err != nil {			t.Fatal(err)
		}
		if !manifest.Supports(ubuntu) {
			t.Errorf("%s does not support first-release target %+v", id, ubuntu)
		}
		for _, platform := range unsupported {
			if manifest.Supports(platform) {
				t.Errorf("%s unexpectedly supports unreviewed target %+v", id, platform)
			}
		}
	}
}
