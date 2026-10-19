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
		if err != nil {
			t.Fatal(err)
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

func TestDatabaseWebToolsStayUnavailableWithoutTrustedSourcesAndNativeAdapters(t *testing.T) {
	catalog, err := BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"phpmyadmin", "pgadmin"} {
		manifest, err := catalog.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if len(manifest.Versions) != 1 || manifest.Versions[0].Source.Kind != "unavailable" || manifest.Verification.Status != "unavailable" {
			t.Errorf("%s still advertises an installable or unverified legacy package: %+v", id, manifest)
		}
		for _, capability := range manifest.Capabilities {
			if capability.Status != "unavailable" {
				t.Errorf("%s capability %s is not hidden as unavailable: %+v", id, capability.Name, capability)
			}
		}
	}
}

func TestSelectedRuntimeCatalogIsVerifiedOnlyOnUbuntu2404AMD64(t *testing.T) {
	catalog, err := BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	ubuntu := Platform{OS: "linux", Arch: "amd64", Distro: "ubuntu", Release: "24.04"}
	for _, id := range []string{"git", "nodejs", "golang"} {
		manifest, err := catalog.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if manifest.Verification.Status != "verified" || !manifest.Supports(ubuntu) {
			t.Errorf("%s lacks Ubuntu 24.04 amd64 verification: %+v", id, manifest.Verification)
		}
		for _, platform := range []Platform{
			{OS: "linux", Arch: "amd64", Distro: "debian", Release: "13"},
			{OS: "linux", Arch: "arm64", Distro: "ubuntu", Release: "24.04"},
		} {
			if manifest.Supports(platform) {
				t.Errorf("%s unexpectedly supports unaccepted platform %+v", id, platform)
			}
		}
	}
}
